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
// 1. AST PHASE: Loop Context Detection (Compiler Aware)
// ---------------------------------------------------------

// LoopRange stores the exact, mathematical start and end position of a loop
// in the abstract syntax tree. Because we use the same token.FileSet for AST and SSA,
// token.Pos perfectly maps between both stages.
type LoopRange struct {
	Start token.Pos
	End   token.Pos
}

type ASTLoopVisitor struct {
	inLoop     bool
	isExecLoop bool
	Executors  map[string]bool
	LoopRanges *[]LoopRange // Pointer to accumulate across all files
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
		LoopRanges: v.LoopRanges, // Carry pointer to inner nodes
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
	targetDir := "example/"
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

	// Step 1: Build Global SSA
	// FIX: Enable GlobalDebug as recommended, along with default NaiveForm,
	// to ensure rigorous AST-to-SSA token.Pos mapping inside instructions.
	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm|ssa.GlobalDebug)
	prog.Build()

	allFuncs := getAllFunctions(initial, prog)

	// Step 2: Dynamically calculate which wrappers are Execution functions
	executors := buildTransitiveExecutors(allFuncs)

	// Step 3: Run AST to map the loops, leveraging the Executor Map
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

	// Step 4: IFDS Tabulation, relying on mathematical token.Pos intersections
	LoopResolutions := Phase1_IFDS_Tabulation(allFuncs, loopRanges, prog.Fset)
	Phase2_VerifyNPlusOne(LoopResolutions)
}

// ---------------------------------------------------------
// IFDS PHASE 1: Dataflow Tracing
// ---------------------------------------------------------

type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}
type ExplodedNode struct {
	Point ProgramPoint
	Fact  ssa.Value
}
type PathEdge struct {
	Start ExplodedNode
	End   ExplodedNode
}
type SummaryCache map[*ssa.Function]map[ssa.Value][]ssa.Value

func Phase1_IFDS_Tabulation(allFunctions []*ssa.Function, loopRanges []LoopRange, ssaFset *token.FileSet) map[string][]ssa.Value {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)
	LoopResolutions := make(map[string][]ssa.Value)

	// HELPER: Checks if an SSA position inherently falls within AST Loop Bounds
	isInLoopBounds := func(pos token.Pos) bool {
		if !pos.IsValid() {
			return false
		}
		for _, lr := range loopRanges {
			if pos >= lr.Start && pos <= lr.End {
				return true
			}
		}
		return false
	}

	// SINK FINDER
	for _, fn := range allFunctions {
		if fn.Synthetic != "" {
			continue // Skip synthetic (compiler-generated) functions
		}

		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok {
					methodName := getCallNameFromInstr(call)
					sinkIndex := getGormFetchArgIndex(methodName)

					// Print override for your custom testing!
					if methodName == "print" || methodName == "DeletePartIteration" {
						fmt.Printf("Found Sink! '%s'\n", methodName)
					}

					if sinkIndex != -1 && len(call.Common().Args) > sinkIndex {
						val := call.Common().Args[sinkIndex]
						callPos := call.Pos()

						// 1. Is the execution physically sitting inside any known loop?
						if isInLoopBounds(callPos) {
							// Translate internal token to human-readable string for output ONLY
							posString := ssaFset.Position(callPos).String()
							LoopResolutions[posString] = append(LoopResolutions[posString], val)
						}

						sink := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
						addPathEdge(&worklist, P_set, PathEdge{Start: sink, End: sink})
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
			if callVal, ok := callInstr.(ssa.Value); ok && callVal == d2 {
				callee := callInstr.Common().StaticCallee()
				if callee != nil {
					for _, block := range callee.Blocks {
						for i, retInstr := range block.Instrs {
							if ret, ok := retInstr.(*ssa.Return); ok {
								newCtx := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: ret.Results[0]}
								addPathEdge(&worklist, P_set, PathEdge{Start: newCtx, End: newCtx})
							}
						}
					}
				}
			} else {
				for _, nd2 := range applyCallToReturn(callInstr, d2) {
					for _, prevPoint := range getPredecessors(v2) {
						addPathEdge(&worklist, P_set, PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}})
					}
				}
			}
		} else if isEntryNode(v2) {
			fn := v2.Block.Parent()
			for paramIdx, param := range fn.Params {
				if param == d2 {
					d1 := edge.Start.Fact
					if summaries[fn] == nil {
						summaries[fn] = make(map[ssa.Value][]ssa.Value)
					}
					summaries[fn][d1] = append(summaries[fn][d1], d2)

					for _, caller := range getGlobalMockCallers(fn, allFunctions) {
						arg := caller.Common().Args[paramIdx]
						callerPos := caller.Pos()

						// 2. Is the mocked caller physically inside any known loop bounds?
						if isInLoopBounds(callerPos) {
							posString := ssaFset.Position(callerPos).String()
							LoopResolutions[posString] = append(LoopResolutions[posString], arg)
						}

						callerPoint := getInstructionPoint(caller)
						for _, prevPoint := range getPredecessors(callerPoint) {
							addPathEdge(&worklist, P_set, PathEdge{
								Start: ExplodedNode{Point: prevPoint, Fact: arg},
								End:   ExplodedNode{Point: prevPoint, Fact: arg},
							})
						}
					}
				}
			}
		} else {
			for _, nd2 := range applyNormalFlow(instr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					addPathEdge(&worklist, P_set, PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}})
				}
			}
		}
	}
	return LoopResolutions
}

// ---------------------------------------------------------
// PHASE 2 & LOGIC UTILITIES
// ---------------------------------------------------------

func Phase2_VerifyNPlusOne(LoopResolutions map[string][]ssa.Value) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: GORM N+1 VULNERABILITY REPORT")
	fmt.Println("==========================================")

	if len(LoopResolutions) == 0 {
		fmt.Println(" ✅ Project Clean! No N+1 Queries detected.")
		return
	}

	vulnsFound := 0

	for locationKey, resolvedArgs := range LoopResolutions {
		isDynamicVariable := false

		// Check if any argument tracing back is NOT a constant
		for _, arg := range resolvedArgs {
			if _, isConst := arg.(*ssa.Const); !isConst {
				isDynamicVariable = true
			}
		}

		if isDynamicVariable {
			vulnsFound++
			fmt.Printf(" 🚨 [TRUE N+1] \t%s \n", locationKey)
		}
	}

	if vulnsFound == 0 {
		fmt.Println(" ✅ Project Clean! All loops use safe, hardcoded static queries.")
	}
}

func buildTransitiveExecutors(funcs []*ssa.Function) map[string]bool {
	execMap := make(map[*ssa.Function]bool)

	// 1. Direct Executors
	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok && isExecutionMethod(getCallNameFromInstr(call)) {
					execMap[fn] = true
				}
			}
		}
	}

	// 2. Transitive Executors
	changed := true
	for changed {
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

func getGormFetchArgIndex(methodName string) int {
	switch methodName {
	case "Where", "Raw", "Not", "Or", "Select":
		return 1
	case "Query", "QueryRow", "Exec":
		return 1
	case "print": // Temp override to allow the logic to analyze your `print` test case!
		return 0
	}
	return -1
}

func applyNormalFlow(instr ssa.Instruction, d2 ssa.Value) []ssa.Value {
	if instrVal, ok := instr.(ssa.Value); ok && instrVal == d2 {
		var newFacts []ssa.Value
		for _, opPtr := range instr.Operands(nil) {
			if opPtr != nil && *opPtr != nil {
				newFacts = append(newFacts, *opPtr)
			}
		}
		return newFacts
	} else if store, ok := instr.(*ssa.Store); ok && store.Addr == d2 {
		return []ssa.Value{store.Val}
	}
	return []ssa.Value{d2}
}

func applyCallToReturn(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		return nil
	}
	return []ssa.Value{fact}
}

func addPathEdge(worklist *[]PathEdge, P_set map[PathEdge]bool, edge PathEdge) {
	if !P_set[edge] {
		P_set[edge] = true
		*worklist = append(*worklist, edge)
	}
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

func getInstructionPoint(instr ssa.Instruction) ProgramPoint {
	b := instr.Block()
	for i, inst := range b.Instrs {
		if inst == instr {
			return ProgramPoint{Block: b, Index: i}
		}
	}
	return ProgramPoint{}
}
