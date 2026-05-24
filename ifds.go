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

type LoopContext struct {
	StartLine int
	EndLine   int
}

type ASTLoopVisitor struct {
	fset          *token.FileSet
	inLoop        bool
	isExecLoop    bool
	currentCtx    LoopContext
	Executors     map[string]bool
	LoopLocations map[string]LoopContext // Filename:Line -> Loop Boundaries
}

func (v *ASTLoopVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}

	isLoop := v.inLoop
	isExecLoop := v.isExecLoop
	ctx := v.currentCtx

	// Set boundaries if entering a loop
	switch loopNode := n.(type) {
	case *ast.ForStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
		ctx = LoopContext{
			StartLine: v.fset.Position(loopNode.Pos()).Line,
			EndLine:   v.fset.Position(loopNode.End()).Line,
		}
	case *ast.RangeStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
		ctx = LoopContext{
			StartLine: v.fset.Position(loopNode.Pos()).Line,
			EndLine:   v.fset.Position(loopNode.End()).Line,
		}
	}

	// HIGHLIGHT ZONE: Mark lines inside Execution loops
	if isLoop && isExecLoop {
		if call, ok := n.(*ast.CallExpr); ok {
			pos := v.fset.Position(call.Pos())
			fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
			v.LoopLocations[fileLineKey] = ctx
		}
	}

	return &ASTLoopVisitor{
		fset:          v.fset,
		inLoop:        isLoop,
		isExecLoop:    isExecLoop,
		currentCtx:    ctx,
		Executors:     v.Executors,
		LoopLocations: v.LoopLocations,
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

			// Call executes if it's a known finisher OR a transitive executor wrapper
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
// MAIN EXECUTION (Project Loader)
// ---------------------------------------------------------

func main() {
	targetDir := "."
	if len(os.Args) > 1 {
		targetDir = os.Args[1]
	}

	fmt.Printf("\n>> LOADING PROJECT: %s\n", targetDir)

	fset := token.NewFileSet()
	cfg := &packages.Config{
		Dir:  targetDir,
		Fset: fset,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}

	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Failed to execute package load: %v", err)
	}

	if packages.PrintErrors(initial) > 0 {
		fmt.Println("[Warning] Some minor package errors occurred (e.g. CGO bindings), continuing anyway...")
	}

	// Step 1: Build Global SSA (No SanityCheck flag intentionally)
	fmt.Println(">> BUILDING GLOBAL SSA DATAFLOW GRAPH...")
	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm)
	prog.Build()

	// Filter down to only user-land functions (ignoring stdlib)
	allFuncs := getAllFunctions(initial, prog)

	// Step 2: Calculate which functions are Transitive Executors
	executors := buildTransitiveExecutors(allFuncs)

	// Step 3: Run AST to map the loops, utilizing Executor context
	fmt.Println(">> RUNNING AST LOOP SCANNER...")
	visitor := &ASTLoopVisitor{
		fset:          fset,
		LoopLocations: make(map[string]LoopContext),
		Executors:     executors,
	}
	for _, pkg := range initial {
		for _, file := range pkg.Syntax {
			ast.Walk(visitor, file)
		}
	}

	// Step 4: Run IFDS Tabulation
	fmt.Println(">> RUNNING IFDS TABULATION ENGINE...")
	LoopResolutions := Phase1_IFDS_Tabulation(allFuncs, visitor.LoopLocations, fset)

	// Step 5: Process Results
	Phase2_VerifyNPlusOne(LoopResolutions, visitor.LoopLocations, fset)
}

// ---------------------------------------------------------
// IFDS DATA STRUCTURES
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

// ---------------------------------------------------------
// 2. IFDS PHASE 1: Dataflow Tracing
// ---------------------------------------------------------
func isGormFetchMethod(methodName string) bool {
	switch methodName {
	// EXPLICIT WHITELIST: Fetch APIs only.
	// We intentionally leave OUT 'Exec', 'Create', 'Delete', 'Update' so they get ignored!
	case "Where", "Raw", "Not", "Or", "Select":
		return true
	case "Query", "QueryRow":
		return true
	}
	return false
}

func Phase1_IFDS_Tabulation(allFunctions []*ssa.Function, loopLocations map[string]LoopContext, fset *token.FileSet) map[string][]ssa.Value {
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

					// If it is a GORM Fetch...
					if isGormFetchMethod(methodName) {

						// LOOP through ALL arguments (skipping arg 0 which is the 'db' receiver)
						for argIdx := 1; argIdx < len(call.Common().Args); argIdx++ {
							val := call.Common().Args[argIdx]

							pos := fset.Position(call.Pos())
							fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

							if loopLocations[fileLineKey].StartLine > 0 {
								LoopResolutions[fileLineKey] = append(LoopResolutions[fileLineKey], val)
							}

							seed := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
							addPathEdge(&worklist, P_set, PathEdge{Start: seed, End: seed})
						}
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

						// BRIDGE 2: Transitive Return traps wrapper loops
						if loopLocations[fileLineKey].StartLine > 0 {
							LoopResolutions[fileLineKey] = append(LoopResolutions[fileLineKey], arg)
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
// 3. PHASE 2: Lookup and False Positive Validation
// ---------------------------------------------------------

func Phase2_VerifyNPlusOne(LoopResolutions map[string][]ssa.Value, LoopLocations map[string]LoopContext, fset *token.FileSet) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: GORM N+1 VULNERABILITY REPORT")
	fmt.Println("==========================================")

	if len(LoopResolutions) == 0 {
		fmt.Println(" ✅ Project Clean! No N+1 Queries detected.")
		return
	}

	vulnsFound := 0

	for locationKey, resolvedArgs := range LoopResolutions {
		loopCtx := LoopLocations[locationKey]

		for _, arg := range resolvedArgs {
			// Ignore constants entirely
			if _, isConst := arg.(*ssa.Const); isConst {
				continue
			}

			// 1. Get the root variable and its exact source line
			rootVar := getRootVariable(arg)
			varLine := fset.Position(rootVar.Pos()).Line

			// 2. STATE POLLING CHECK
			// If variable was created before the loop started, it's just Polling/Status Checking!
			if varLine > 0 && varLine < loopCtx.StartLine {
				// (Optional: You can uncomment this if you want to see State Polling reported)
				// fmt.Printf(" ✅ [STATE POLLING] \t%s \n\t-> Variable '%s' was defined outside the loop (Line %d).\n\n",
				//	locationKey, resolveHumanName(arg), varLine)
				continue
			}

			// 3. TRUE N+1
			vulnsFound++
			humanName := resolveHumanName(arg)
			fmt.Printf(" 🚨 [TRUE N+1] \t%s \n\t-> Loop dynamically fetches using iteration variable %s\n\n", locationKey, humanName)
		}
	}

	if vulnsFound == 0 {
		fmt.Println(" ✅ Project Clean! All loops use safe state polling or hardcoded queries.")
	}
}

// ============================================================================
// STATE POLLING & VARIABLE RESOLUTION
// ============================================================================

// Recursively walks back registers to find the exact line
// the variable was instantiated (to combat State Polling)
func getRootVariable(val ssa.Value) ssa.Value {
	visited := make(map[ssa.Value]bool)
	var dfs func(v ssa.Value) ssa.Value
	dfs = func(v ssa.Value) ssa.Value {
		if v == nil || visited[v] {
			return v
		}
		visited[v] = true

		if _, ok := v.(*ssa.Alloc); ok {
			return v
		}
		if _, ok := v.(*ssa.Parameter); ok {
			return v
		}
		if _, ok := v.(*ssa.Global); ok {
			return v
		}
		if _, ok := v.(*ssa.FreeVar); ok {
			return v
		}
		if _, ok := v.(*ssa.Const); ok {
			return v
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
			return dfs(x.X)
		case *ssa.IndexAddr:
			return dfs(x.X)
		}
		return v
	}
	return dfs(val)
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

// ============================================================================
// GLOBAL PROJECT LOGIC AND SINK FINDERS
// ============================================================================

func buildTransitiveExecutors(funcs []*ssa.Function) map[string]bool {
	execMap := make(map[*ssa.Function]bool)

	// Base Execution Methods
	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok && isExecutionMethod(getCallNameFromInstr(call)) {
					execMap[fn] = true
				}
			}
		}
	}

	// Wrapper Propagation
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

func isEntryNode(node ProgramPoint) bool {
	return node.Index == 0 && node.Block.Index == 0
}

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
