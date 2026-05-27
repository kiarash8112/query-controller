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
// 1. AST PHASE: Loop Context Detection & Bounding Boxes
// ---------------------------------------------------------

type LoopContext struct {
	StartLine int
	EndLine   int
}

type ASTLoopVisitor struct {
	fset          *token.FileSet
	Executors     map[string]bool
	LoopLocations map[string][]LoopContext // Filename -> Slice of loop bounds inside that file
}

func (v *ASTLoopVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}

	switch loopNode := n.(type) {
	case *ast.ForStmt, *ast.RangeStmt:
		// 1. Check if the loop triggers an execution
		var loopBody ast.Node
		if f, ok := loopNode.(*ast.ForStmt); ok {
			loopBody = f.Body
		}
		if r, ok := loopNode.(*ast.RangeStmt); ok {
			loopBody = r.Body
		}

		if astContainsExecutionMethod(loopBody, v.Executors) {
			// 2. Register the Bounding Box
			startPos := v.fset.Position(loopNode.Pos())
			endPos := v.fset.Position(loopNode.End())

			box := LoopContext{
				StartLine: startPos.Line,
				EndLine:   endPos.Line,
			}
			v.LoopLocations[startPos.Filename] = append(v.LoopLocations[startPos.Filename], box)
		}
	}

	// Just pass the maps down, no state needed!
	return v
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
		fmt.Println("[Warning] Minor package errors occurred, continuing analysis...")
	}

	fmt.Println(">> BUILDING GLOBAL SSA DATAFLOW GRAPH...")
	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm)
	prog.Build()

	allFuncs := getAllFunctions(initial, prog)
	executors := buildTransitiveExecutors(allFuncs)

	fmt.Println(">> RUNNING AST LOOP SCANNER...")
	visitor := &ASTLoopVisitor{
		fset:          fset,
		LoopLocations: make(map[string][]LoopContext),
		Executors:     executors,
	}
	for _, pkg := range initial {
		for _, file := range pkg.Syntax {
			ast.Walk(visitor, file)
		}
	}

	fmt.Println(">> RUNNING IFDS TABULATION ENGINE...")
	P_set := Phase1_IFDS_Tabulation(allFuncs, visitor.LoopLocations, fset)

	Phase2_VerifyNPlusOne(P_set, visitor.LoopLocations, fset)
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
type SummaryCache map[*ssa.Function]map[ssa.Value][]ssa.Value

type PathEdge struct {
	Start      ExplodedNode
	End        ExplodedNode
	OriginLoop string // Carries the Loop Bounding Box context through wrappers!
}

// ---------------------------------------------------------
// 2. IFDS PHASE 1: Dataflow Tracing
// ---------------------------------------------------------

func Phase1_IFDS_Tabulation(allFunctions []*ssa.Function, loopLocations map[string][]LoopContext, fset *token.FileSet) map[PathEdge]bool {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)

	// SINK FINDER
	for _, fn := range allFunctions {
		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				if call, ok := instr.(*ssa.Call); ok {
					methodName := getCallNameFromInstr(call)
					if isGormFetchCondtion(methodName) {
						// Track ALL args passed to the query!
						for argIdx := 1; argIdx < len(call.Common().Args); argIdx++ {
							val := call.Common().Args[argIdx]

							p := ProgramPoint{Block: block, Index: i}
							tag := getLoopTag("", p, fset, loopLocations)

							seed := ExplodedNode{Point: p, Fact: val}
							addPathEdge(&worklist, P_set, PathEdge{Start: seed, End: seed, OriginLoop: tag})
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

			// Resolve Extract Index for Multi-Return Functions
			trackIndex := 0
			if callVal, isVal := callInstr.(ssa.Value); isVal {
				if referrers := callVal.Referrers(); referrers != nil {
					for _, ref := range *referrers {
						if extract, isExtract := ref.(*ssa.Extract); isExtract && extract == d2 {
							trackIndex = extract.Index
						}
					}
				}
			}

			// A. DIVE INTO METHOD (CallFlow)
			if callVal, ok := callInstr.(ssa.Value); ok && (callVal == d2 || callVal.Type().Underlying().String() == "tuple") {
				callee := callInstr.Common().StaticCallee()
				if callee != nil {

					// SUMMARY CACHE CHECK
					summaryUsed := false
					if cachedResults, exists := summaries[callee][d2]; exists {
						for _, paramD2 := range cachedResults {
							for i, param := range callee.Params {
								if param == paramD2 {
									arg := callInstr.Common().Args[i]
									for _, prevPoint := range getPredecessors(v2) {
										newTag := getLoopTag(edge.OriginLoop, prevPoint, fset, loopLocations)
										addPathEdge(&worklist, P_set, PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: arg}, OriginLoop: newTag})
									}
								}
							}
						}
						summaryUsed = true
					}

					// FUNCTION DIVE
					if !summaryUsed {
						for _, block := range callee.Blocks {
							for i, retInstr := range block.Instrs {
								if ret, ok := retInstr.(*ssa.Return); ok {
									safeIndex := trackIndex
									if safeIndex >= len(ret.Results) {
										safeIndex = 0
									}

									newCtx := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: ret.Results[safeIndex]}
									addPathEdge(&worklist, P_set, PathEdge{Start: newCtx, End: newCtx, OriginLoop: edge.OriginLoop})
								}
							}
						}
					}
				}
			} else {
				// B. BYPASS FUNCTION (CallToReturn)
				for _, nd2 := range byPassFunction(callInstr, d2) {
					for _, prevPoint := range getPredecessors(v2) {
						newTag := getLoopTag(edge.OriginLoop, prevPoint, fset, loopLocations)
						addPathEdge(&worklist, P_set, PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}, OriginLoop: newTag})
					}
				}
			}
		} else if isEntryNode(v2) {
			// C. EXIT FUNCTION (ReturnFlow)
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
						callerPoint := getInstructionPoint(caller)
						for _, prevPoint := range getPredecessors(callerPoint) {
							newTag := getLoopTag(edge.OriginLoop, prevPoint, fset, loopLocations)
							addPathEdge(&worklist, P_set, PathEdge{Start: ExplodedNode{Point: prevPoint, Fact: arg}, End: ExplodedNode{Point: prevPoint, Fact: arg}, OriginLoop: newTag})
						}
					}
				}
			}
		} else {
			// D. STANDARD FLOW (Kill/Gen)
			for _, nd2 := range applyNormalFlow(instr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					newTag := getLoopTag(edge.OriginLoop, prevPoint, fset, loopLocations)
					addPathEdge(&worklist, P_set, PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}, OriginLoop: newTag})
				}
			}
		}
	}
	return P_set
}

// ---------------------------------------------------------
// 3. PHASE 2: Lookup and False Positive Validation
// ---------------------------------------------------------

func Phase2_VerifyNPlusOne(P_set map[PathEdge]bool, loopLocs map[string][]LoopContext, fset *token.FileSet) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: GORM N+1 VULNERABILITY REPORT")
	fmt.Println("==========================================")

	LoopTerminals := make(map[string][]ssa.Value)
	dedup := make(map[string]map[ssa.Value]bool)

	for edge := range P_set {
		if edge.OriginLoop == "" {
			continue
		}
		fact := edge.End.Fact

		switch fact.(type) {
		case *ssa.Const, *ssa.Alloc, *ssa.Parameter, *ssa.Global, *ssa.FreeVar:
			if dedup[edge.OriginLoop] == nil {
				dedup[edge.OriginLoop] = make(map[ssa.Value]bool)
			}
			if !dedup[edge.OriginLoop][fact] {
				dedup[edge.OriginLoop][fact] = true
				LoopTerminals[edge.OriginLoop] = append(LoopTerminals[edge.OriginLoop], fact)
			}
		}
	}

	if len(LoopTerminals) == 0 {
		fmt.Println(" ✅ Project Clean! No N+1 Queries detected in loops.")
		return
	}

	vulnsFound := 0
	for locationKey, roots := range LoopTerminals {
		isDynamicVariable := false
		var dynamicVal ssa.Value
		var constVal string

		for _, root := range roots {
			if c, isConst := root.(*ssa.Const); isConst {
				// FIX: Safely check if the constant is literally the Go 'nil' keyword
				if c.Value == nil {
					constVal = "nil"
				} else {
					constVal = c.Value.ExactString()
				}
			} else {
				isDynamicVariable = true
				dynamicVal = root
			}
		}

		if !isDynamicVariable {
			fmt.Printf(" ✅ [SAFE] \t%s \n\t-> Safe Static Dataflow: %s\n\n", locationKey, constVal)
		} else {

			// FIX: Safely check if dynamicVal or its root variable is nil
			var varLine int
			if dynamicVal != nil {
				rootVar := getRootVariable(dynamicVal)
				if rootVar != nil {
					varLine = fset.Position(rootVar.Pos()).Line
				}
			}

			var boundCtx *LoopContext
			for filename, boxes := range loopLocs {
				for _, box := range boxes {
					if fmt.Sprintf("%s:%d", filename, box.StartLine) == locationKey {
						boundCtx = &box
					}
				}
			}

			if boundCtx != nil && varLine > 0 && varLine < boundCtx.StartLine {
				// SAFE: State polling
				continue
			}

			vulnsFound++
			humanName := resolveHumanName(dynamicVal)
			fmt.Printf(" 🚨 [TRUE N+1] \t%s \n\t-> Dynamically fetches using variable: %s\n\n", locationKey, humanName)
		}
	}
	if vulnsFound == 0 {
		fmt.Println(" ✅ Project Clean! All loops use state polling or static queries.")
	}
}

// ============================================================================
// LOGIC UTILITIES & TAGGING
// ============================================================================

func getLoopTag(currentTag string, p ProgramPoint, fset *token.FileSet, locs map[string][]LoopContext) string {
	if currentTag != "" {
		return currentTag
	}

	if p.Block == nil || len(p.Block.Instrs) == 0 {
		return ""
	}
	idx := p.Index
	if idx >= len(p.Block.Instrs) {
		idx = len(p.Block.Instrs) - 1
	}

	pos := fset.Position(p.Block.Instrs[idx].Pos())
	if ctx := isInsideLoop(pos.Filename, pos.Line, locs); ctx != nil {
		return fmt.Sprintf("%s:%d", pos.Filename, ctx.StartLine)
	}
	return ""
}

func isInsideLoop(filename string, line int, loopLocations map[string][]LoopContext) *LoopContext {
	boxes := loopLocations[filename]
	for _, box := range boxes {
		if line >= box.StartLine && line <= box.EndLine {
			return &box
		}
	}
	return nil
}

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
	// FIX: Instant safety check!
	if val == nil {
		return "'unresolved_nil'"
	}

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

	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok && isExecutionMethod(getCallNameFromInstr(call)) {
					execMap[fn] = true
				}
			}
		}
	}

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

func isGormFetchCondtion(methodName string) bool {
	switch methodName {
	case "Where", "Raw", "Not", "Or", "Select", "Query", "QueryRow":
		return true
	}
	return false
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

func byPassFunction(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
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

