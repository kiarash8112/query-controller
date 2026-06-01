package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"os"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// ---------------------------------------------------------
// 1. AST PHASE: Loop Context Detection
// ---------------------------------------------------------

type LoopRange struct {
	Start token.Pos
	End   token.Pos
}

type ASTLoopVisitor struct {
	inLoop     bool
	isExecLoop bool
	Executors  map[string]bool
	LoopRanges *[]LoopRange
}

func (v *ASTLoopVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}

	isLoop := v.inLoop
	isExecLoop := v.isExecLoop

	switch loopNode := n.(type) {
	case *ast.ForStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
		if isExecLoop {
			*v.LoopRanges = append(*v.LoopRanges, LoopRange{Start: loopNode.Pos(), End: loopNode.End()})
		}
	case *ast.RangeStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
		if isExecLoop {
			*v.LoopRanges = append(*v.LoopRanges, LoopRange{Start: loopNode.Pos(), End: loopNode.End()})
		}
	}

	return &ASTLoopVisitor{
		inLoop:     isLoop,
		isExecLoop: isExecLoop,
		Executors:  v.Executors,
		LoopRanges: v.LoopRanges,
	}
}

func astContainsExecutionMethod(node ast.Node, executors map[string]bool) bool {
	hasExecution := false
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			var name string
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				name = sel.Sel.Name
			} else if ident, ok := call.Fun.(*ast.Ident); ok {
				name = ident.Name
			}

			if isExecutionMethod(name) || executors[name] {
				hasExecution = true
				return false
			}
		}
		return true
	})
	return hasExecution
}

// ---------------------------------------------------------
// MAIN EXECUTION
// ---------------------------------------------------------

func main() {
	targetDir := "examples/"
	if len(os.Args) > 1 {
		targetDir = os.Args[1]
	}
	fmt.Printf(">> LOADING PROJECT: %s\n", targetDir)

	fset := token.NewFileSet()
	cfg := &packages.Config{
		Fset: fset,
		Dir:  targetDir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}

	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Load failed: %v", err)
	}

	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm|ssa.GlobalDebug)
	prog.Build()

	allFuncs := getAllFunctions(initial, prog)
	executors := buildTransitiveExecutors(allFuncs)

	loopRanges := make([]LoopRange, 0)
	visitor := &ASTLoopVisitor{
		Executors:  executors,
		LoopRanges: &loopRanges,
	}
	for _, pkg := range initial {
		for _, file := range pkg.Syntax {
			ast.Walk(visitor, file)
		}
	}

	// Phase 1: Context-Aware Dataflow Trace (Pure Stack)
	AllResolutions := Phase1_IFDS_Tabulation(allFuncs)

	// Phase 2: Vulnerability Checker
	Phase2_VerifyNPlusOne(AllResolutions, loopRanges, prog.Fset)
}

// ---------------------------------------------------------
// IFDS PHASE 1: Pure Dataflow Tracing (Call Stack Context)
// ---------------------------------------------------------

const MaxCallStackDepth = 7 // Prevent infinite recursive tracing (k-CFA limit)

type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}
type ExplodedNode struct {
	Point ProgramPoint
	Fact  ssa.Value
}
type StackFrame struct {
	Call  ssa.CallInstruction
	Start ExplodedNode
}
type PathEdge struct {
	Start     ExplodedNode
	End       ExplodedNode
	CallStack []StackFrame
}

// Helper to safely clone the stack slice to avoid pointer overwrites
func cloneStack(stack []StackFrame) []StackFrame {
	if len(stack) == 0 {
		return nil
	}
	ns := make([]StackFrame, len(stack))
	copy(ns, stack)
	return ns
}

// Helper to hash edges for our P_set
func hashEdge(e PathEdge) string {
	stackHash := ""
	for _, s := range e.CallStack {
		stackHash += fmt.Sprintf("[%p]", s.Call)
	}
	return fmt.Sprintf("S:%p:%d:%p|E:%p:%d:%p|ST:%s",
		e.Start.Point.Block, e.Start.Point.Index, e.Start.Fact,
		e.End.Point.Block, e.End.Point.Index, e.End.Fact, stackHash)
}

func Phase1_IFDS_Tabulation(allFunctions []*ssa.Function) map[token.Pos][]ssa.Value {
	P_set := make(map[string]bool)
	var worklist []PathEdge

	AllResolutions := make(map[token.Pos][]ssa.Value)

	addPathEdge := func(edge PathEdge) {
		key := hashEdge(edge)
		if !P_set[key] {
			P_set[key] = true
			worklist = append(worklist, edge)
		}
	}

	// SINK FINDER (Initializes with Empty Stack)
	for _, fn := range allFunctions {
		if fn.Synthetic != "" {
			continue
		}
		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok {
					targetArgs := getGormSinkArgs(call)
					for _, val := range targetArgs {
						if call.Pos().IsValid() {
							AllResolutions[call.Pos()] = append(AllResolutions[call.Pos()], val)
						}
						sink := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
						addPathEdge(PathEdge{Start: sink, End: sink, CallStack: nil}) // Empty stack!
					}
				}
			}
		}
	}

	// TABULATION ENGINE
	for len(worklist) > 0 {
		edge := worklist[0]
		worklist = worklist[1:]

		v2 := edge.End.Point
		d2 := edge.End.Fact
		instr := v2.Block.Instrs[v2.Index]

		if callInstr, ok := instr.(ssa.CallInstruction); ok {

			// SCENARIO 1: Tracked variable comes from a Function Call (e.g. id := SafeFunc())
			if callVal, ok := callInstr.(ssa.Value); ok && callVal == d2 {
				callee := callInstr.Common().StaticCallee()
				if callee != nil {

					// --- FIX FOR INFINITE LOOPS ---
					// Prevent unbounded stack growth in the presence of recursion.
					if len(edge.CallStack) >= MaxCallStackDepth {
						continue // Drop tracing this path; we've gone too deep
					}
					// ------------------------------

					for _, block := range callee.Blocks {
						for i, retInstr := range block.Instrs {
							if ret, ok := retInstr.(*ssa.Return); ok && len(ret.Results) > 0 {
								calleeD1 := ret.Results[0]

								// Start tracing inside the child, push Caller onto Stack
								newCtx := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: calleeD1}

								newStack := cloneStack(edge.CallStack)
								newStack = append(newStack, StackFrame{
									Call:  callInstr,
									Start: edge.Start, // Safely save the caller's start context!
								})

								addPathEdge(PathEdge{Start: newCtx, End: newCtx, CallStack: newStack})
							}
						}
					}
				}
			} else {
				// Bypass instruction entirely if it's not what we are tracking
				for _, nd2 := range applyCallToReturn(callInstr, d2) {
					for _, prevPoint := range getPredecessors(v2) {
						addPathEdge(PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}, CallStack: cloneStack(edge.CallStack)})
					}
				}
			}

			// SCENARIO 2: Reached the Top of a Function (Entry Parameters)
		} else if isEntryNode(v2) {
			fn := v2.Block.Parent()
			for paramIdx, param := range fn.Params {
				if param == d2 {

					// 2A) POP THE STACK: We entered to fetch data. Context restores to specific caller.
					if len(edge.CallStack) > 0 {
						lastFrame := edge.CallStack[len(edge.CallStack)-1]
						newStack := cloneStack(edge.CallStack[:len(edge.CallStack)-1]) // POP

						callerInstr := lastFrame.Call
						if len(callerInstr.Common().Args) > paramIdx {
							actualArg := callerInstr.Common().Args[paramIdx]

							if callerInstr.Pos().IsValid() {
								AllResolutions[callerInstr.Pos()] = append(AllResolutions[callerInstr.Pos()], actualArg)
							}
							callerPoint := getInstructionPoint(callerInstr)
							for _, prevPoint := range getPredecessors(callerPoint) {
								addPathEdge(PathEdge{
									Start:     lastFrame.Start, // Restoring original Start Node safely!
									End:       ExplodedNode{Point: prevPoint, Fact: actualArg},
									CallStack: newStack,
								})
							}
						}
					} else {
						// 2B) STACK IS EMPTY: This is a Sink-Wrapper that needs to propagate backwards (e.g. GetUser)
						for _, caller := range getGlobalMockCallers(fn, allFunctions) {
							if len(caller.Common().Args) > paramIdx {
								arg := caller.Common().Args[paramIdx]

								if caller.Pos().IsValid() {
									AllResolutions[caller.Pos()] = append(AllResolutions[caller.Pos()], arg)
								}
								callerPoint := getInstructionPoint(caller)
								for _, prevPoint := range getPredecessors(callerPoint) {
									addPathEdge(PathEdge{
										Start:     ExplodedNode{Point: prevPoint, Fact: arg},
										End:       ExplodedNode{Point: prevPoint, Fact: arg},
										CallStack: nil, // Still propagating up as a root sink track!
									})
								}
							}
						}
					}
				}
			}
		} else {
			// Normal Instruction Flow (Assignments, Constants, math)
			for _, nd2 := range applyNormalFlow(instr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					addPathEdge(PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}, CallStack: cloneStack(edge.CallStack)})
				}
			}
		}
	}
	return AllResolutions
}

// ---------------------------------------------------------
// DATAFLOW ENHANCED FOR SLICES/VARIADICS
// ---------------------------------------------------------

func applyNormalFlow(instr ssa.Instruction, d2 ssa.Value) []ssa.Value {
	if instrVal, ok := instr.(ssa.Value); ok && instrVal == d2 {
		var newFacts []ssa.Value
		for _, opPtr := range instr.Operands(nil) {
			if opPtr != nil && *opPtr != nil {
				newFacts = append(newFacts, *opPtr)
			}
		}
		return newFacts
	}

	if store, ok := instr.(*ssa.Store); ok {
		if store.Addr == d2 {
			return []ssa.Value{d2, store.Val}
		}
		if idx, isIdx := store.Addr.(*ssa.IndexAddr); isIdx && idx.X == d2 {
			return []ssa.Value{d2, store.Val}
		}
		if fld, isFld := store.Addr.(*ssa.FieldAddr); isFld && fld.X == d2 {
			return []ssa.Value{d2, store.Val}
		}
	}
	return []ssa.Value{d2}
}

// ---------------------------------------------------------
// PHASE 2 & SINK UTILITIES
// ---------------------------------------------------------

func getGormSinkArgs(call ssa.CallInstruction) []ssa.Value {
	methodName := getCallNameFromInstr(call)
	switch methodName {
	case "Where", "Raw", "Not", "Or", "Select", "Having", "Group", "Order", "Query", "QueryRow", "Exec":
		if len(call.Common().Args) > 1 {
			return call.Common().Args[1:]
		}
	case "print":
		if len(call.Common().Args) > 0 {
			return call.Common().Args[:]
		}
	}
	return nil
}

func Phase2_VerifyNPlusOne(AllResolutions map[token.Pos][]ssa.Value, loopRanges []LoopRange, fset *token.FileSet) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: GORM N+1 VULNERABILITY REPORT")
	fmt.Println("==========================================")

	vulnsFound := 0

	isInLoopBounds := func(pos token.Pos) bool {
		for _, lr := range loopRanges {
			if pos >= lr.Start && pos <= lr.End {
				return true
			}
		}
		return false
	}

	for pos, resolvedArgs := range AllResolutions {
		if !isInLoopBounds(pos) {
			continue
		}

		isDynamicVariable := false
		for _, arg := range resolvedArgs {
			if _, isConst := arg.(*ssa.Const); !isConst {
				isDynamicVariable = true
			}
		}

		if isDynamicVariable {
			vulnsFound++
			posString := fset.Position(pos).String()
			fmt.Printf(" 🚨 [TRUE N+1] Found dynamic database execution in loop at \t%s \n", posString)
		}
	}

	if vulnsFound == 0 {
		fmt.Println(" ✅ Project Clean! No N+1 Queries detected.")
	}
}

func buildTransitiveExecutors(funcs []*ssa.Function) map[string]bool {
	execMap := make(map[*ssa.Function]bool)

	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok && isExecutionMethod(getCallNameFromInstr(call)) {
					execMap[fn] = true
				}
			}
		}
	}

	for changed := true; changed; {
		changed = false
		for _, fn := range funcs {
			if execMap[fn] {
				continue
			}
			for _, b := range fn.Blocks {
				for _, instr := range b.Instrs {
					if call, ok := instr.(ssa.CallInstruction); ok {
						if callee := call.Common().StaticCallee(); callee != nil && execMap[callee] {
							execMap[fn] = true
							changed = true
						}
					}
				}
			}
		}
	}

	names := make(map[string]bool)
	for fn := range execMap {
		names[fn.Name()] = true
	}
	return names
}

func getAllFunctions(initial []*packages.Package, prog *ssa.Program) []*ssa.Function {
	var funcs []*ssa.Function
	userPkgs := make(map[*types.Package]bool)
	for _, p := range initial {
		if p.Types != nil {
			userPkgs[p.Types] = true
		}
	}

	for _, pkg := range prog.AllPackages() {
		if pkg == nil || pkg.Pkg == nil || !userPkgs[pkg.Pkg] {
			continue
		}
		for _, mem := range pkg.Members {
			if fn, ok := mem.(*ssa.Function); ok {
				funcs = append(funcs, fn)
			}
			if t, ok := mem.(*ssa.Type); ok {
				mset := prog.MethodSets.MethodSet(t.Type())
				for i := 0; i < mset.Len(); i++ {
					if fn := prog.MethodValue(mset.At(i)); fn != nil {
						funcs = append(funcs, fn)
					}
				}
				ptrMset := prog.MethodSets.MethodSet(types.NewPointer(t.Type()))
				for i := 0; i < ptrMset.Len(); i++ {
					if fn := prog.MethodValue(ptrMset.At(i)); fn != nil {
						funcs = append(funcs, fn)
					}
				}
			}
		}
	}
	return funcs
}

func isExecutionMethod(name string) bool {
	switch name {
	case "Scan", "Find", "First", "Take", "Last", "Pluck", "Count", "Exec", "QueryRow", "Query", "print":
		return true
	}
	return false
}

func getCallNameFromInstr(call ssa.CallInstruction) string {
	if call.Common().Method != nil {
		return call.Common().Method.Name()
	}
	if callee := call.Common().StaticCallee(); callee != nil {
		return callee.Name()
	}
	return ""
}

func applyCallToReturn(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		return nil
	}
	return []ssa.Value{fact}
}

func isEntryNode(node ProgramPoint) bool { return node.Index == 0 && node.Block.Index == 0 }

func getPredecessors(p ProgramPoint) []ProgramPoint {
	if p.Index > 0 {
		return []ProgramPoint{{Block: p.Block, Index: p.Index - 1}}
	}
	var preds []ProgramPoint
	for _, predBlock := range p.Block.Preds {
		if len(predBlock.Instrs) > 0 {
			preds = append(preds, ProgramPoint{Block: predBlock, Index: len(predBlock.Instrs) - 1})
		}
	}
	return preds
}

func getInstructionPoint(instr ssa.Instruction) ProgramPoint {
	b := instr.Block()
	for i, inst := range b.Instrs {
		if inst == instr {
			return ProgramPoint{Block: b, Index: i}
		}
	}
	return ProgramPoint{}
}

func getGlobalMockCallers(fn *ssa.Function, allFunctions []*ssa.Function) []ssa.CallInstruction {
	var callers []ssa.CallInstruction
	for _, cFn := range allFunctions {
		for _, b := range cFn.Blocks {
			for _, inst := range b.Instrs {
				if callInst, ok := inst.(ssa.CallInstruction); ok && callInst.Common().StaticCallee() == fn {
					callers = append(callers, callInst)
				}
			}
		}
	}
	return callers
}
