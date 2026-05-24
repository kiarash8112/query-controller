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
// 1. AST PHASE: Loop Context Detection (With Executor Map)
// ---------------------------------------------------------

type ASTLoopVisitor struct {
	fset          *token.FileSet
	inLoop        bool
	isExecLoop    bool
	Executors     map[string]bool // Knows exactly which wrappers execute!
	LoopLocations map[string]bool
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
	case *ast.RangeStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
	}

	// HIGHLIGHT ZONE: Mark every single line inside the loop as an "Execution Zone"
	if isLoop && isExecLoop {
		pos := v.fset.Position(n.Pos())
		fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
		v.LoopLocations[fileLineKey] = true
	}

	return &ASTLoopVisitor{fset: v.fset, inLoop: isLoop, isExecLoop: isExecLoop, Executors: v.Executors, LoopLocations: v.LoopLocations}
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

			// Does it call a Known Executor OR a Transitive Wrapper?
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
	targetDir := "."
	if len(os.Args) > 1 {
		targetDir = os.Args[1]
	}
	fmt.Printf(">> LOADING PROJECT: %s\n", targetDir)

	fset := token.NewFileSet()
	cfg := &packages.Config{
		Dir: targetDir, Fset: fset,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}

	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Load failed: %v", err)
	}

	// Step 1: Build Global SSA First!
	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm)
	prog.Build()

	allFuncs := getAllFunctions(initial, prog)

	// Step 2: Dynamically calculate which wrappers are Execution functions
	executors := buildTransitiveExecutors(allFuncs)

	// Step 3: Run AST to map the loops, leveraging the Executor Map!
	visitor := &ASTLoopVisitor{fset: fset, LoopLocations: make(map[string]bool), Executors: executors}
	for _, pkg := range initial {
		for _, file := range pkg.Syntax {
			ast.Walk(visitor, file)
		}
	}

	// Step 4: Run IFDS
	LoopResolutions := Phase1_IFDS_Tabulation(allFuncs, visitor.LoopLocations, fset)
	Phase2_VerifyNPlusOne(LoopResolutions, visitor.LoopLocations)
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

func Phase1_IFDS_Tabulation(allFunctions []*ssa.Function, loopLocations map[string]bool, fset *token.FileSet) map[string][]ssa.Value {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)
	LoopResolutions := make(map[string][]ssa.Value)

	// SINK FINDER
	for _, fn := range allFunctions {
		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				if call, ok := instr.(*ssa.Call); ok {
					methodName := getCallNameFromInstr(call)
					sinkIndex := getGormFetchArgIndex(methodName)

					if sinkIndex != -1 && len(call.Common().Args) > sinkIndex {
						val := call.Common().Args[sinkIndex]

						pos := fset.Position(call.Pos())
						fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

						if loopLocations[fileLineKey] {
							LoopResolutions[fileLineKey] = append(LoopResolutions[fileLineKey], val)
						}

						seed := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
						addPathEdge(&worklist, P_set, PathEdge{Start: seed, End: seed})
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

						pos := fset.Position(caller.Pos())
						fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

						if loopLocations[fileLineKey] {
							LoopResolutions[fileLineKey] = append(LoopResolutions[fileLineKey], arg)
						}

						callerPoint := getInstructionPoint(caller)
						for _, prevPoint := range getPredecessors(callerPoint) {
							addPathEdge(&worklist, P_set, PathEdge{Start: ExplodedNode{Point: prevPoint, Fact: arg}, End: ExplodedNode{Point: prevPoint, Fact: arg}})
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

func Phase2_VerifyNPlusOne(LoopResolutions map[string][]ssa.Value, LoopLocations map[string]bool) {
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
		var dynamicVal ssa.Value

		for _, arg := range resolvedArgs {
			if _, isConst := arg.(*ssa.Const); !isConst {
				isDynamicVariable = true
				dynamicVal = arg
			}
		}

		if isDynamicVariable {
			vulnsFound++
			humanName := resolveHumanName(dynamicVal)
			fmt.Printf(" 🚨 [TRUE N+1] \t%s \n\t-> Loop dynamically fetches using variable: %s\n\n", locationKey, humanName)
		}
	}

	if vulnsFound == 0 {
		fmt.Println(" ✅ Project Clean! All loops use safe, hardcoded static queries.")
	}
}

// Builds the list of functions that transitively trigger DB network requests.
func buildTransitiveExecutors(funcs []*ssa.Function) map[string]bool {
	execMap := make(map[*ssa.Function]bool)

	// 1. Direct Executors (Scan, Find, Query)
	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok && isExecutionMethod(getCallNameFromInstr(call)) {
					execMap[fn] = true
				}
			}
		}
	}

	// 2. Transitive Executors (Wrappers calling Executors)
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

// Filters out unneeded Standard Library dependencies directly
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
	case "Scan", "Find", "First", "Take", "Last", "Pluck", "Count", "Exec", "QueryRow", "Query":
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
	}
	return -1
}

func resolveHumanName(val ssa.Value) string {
	visited := make(map[ssa.Value]bool)
	var dfs func(v ssa.Value) string
	dfs = func(v ssa.Value) string {
		if v == nil || visited[v] {
			return ""
		}
		visited[v] = true
		if alloc, ok := v.(*ssa.Alloc); ok && alloc.Comment != "" {
			return alloc.Comment
		}
		if param, ok := v.(*ssa.Parameter); ok && param.Name() != "" {
			return param.Name()
		}
		if global, ok := v.(*ssa.Global); ok && global.Name() != "" {
			return global.Name()
		}
		if fv, ok := v.(*ssa.FreeVar); ok && fv.Name() != "" {
			return fv.Name()
		}
		switch x := v.(type) {
		case *ssa.MakeInterface:
			return dfs(x.X)
		case *ssa.ChangeType:
			return dfs(x.X)
		case *ssa.Slice:
			return dfs(x.X)
		case *ssa.UnOp:
			return dfs(x.X)
		case *ssa.Extract:
			return dfs(x.Tuple)
		case *ssa.FieldAddr:
			if res := dfs(x.X); res != "" {
				return res + " (struct field)"
			}
		case *ssa.IndexAddr:
			if res := dfs(x.X); res != "" {
				return res + " (slice item)"
			}
		}
		return ""
	}
	if name := dfs(val); name != "" {
		return "'" + name + "'"
	}
	if val.Name() != "" {
		return "'" + val.Name() + "'"
	}
	return fmt.Sprintf("%T", val)
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
