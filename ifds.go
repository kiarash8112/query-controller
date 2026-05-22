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
// 1. AST PHASE: Global Loop Context Detection
// ---------------------------------------------------------

type ASTLoopVisitor struct {
	fset          *token.FileSet
	inLoop        bool
	LoopLocations map[string]string // "Filename:Line" -> "Called Function Name"
}

func (v *ASTLoopVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}

	isLoop := v.inLoop
	switch n.(type) {
	case *ast.ForStmt, *ast.RangeStmt:
		isLoop = true
	}

	if isLoop {
		if call, ok := n.(*ast.CallExpr); ok {
			var name string
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}

			// BRIDGE SETUP: Use "Filename:Line" to prevent collisions across a large project
			pos := v.fset.Position(call.Pos())
			fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

			v.LoopLocations[fileLineKey] = name
		}
	}
	return &ASTLoopVisitor{fset: v.fset, inLoop: isLoop, LoopLocations: v.LoopLocations}
}

// ---------------------------------------------------------
// MAIN EXECUTION (Project Loader)
// ---------------------------------------------------------
func main() {
	// Parse target directory from arguments, default to current dir
	targetDir := "/home/user/Desktop/r2basic"
	if len(os.Args) > 1 {
		targetDir = os.Args[1]
	}

	fmt.Printf(">> LOADING PROJECT: %s\n", targetDir)

	fset := token.NewFileSet()

	// FIX 1: Set "Dir" to the target project so it uses the target's go.mod!
	cfg := &packages.Config{
		Dir:  targetDir,
		Fset: fset,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}

	// FIX 2: Load "./..." from inside that specific target Directory
	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Failed to execute package load: %v", err)
	}

	// FIX 3: Don't Fatal exit! Print errors, but continue analyzing whatever successfully loaded.
	if packages.PrintErrors(initial) > 0 {
		fmt.Println("[Warning] Some package errors occurred, but continuing analysis on successfully loaded files...")
	}

	// Step 2: Global AST Scan
	visitor := &ASTLoopVisitor{fset: fset, LoopLocations: make(map[string]string)}
	for _, pkg := range initial {
		for _, file := range pkg.Syntax {
			ast.Walk(visitor, file)
		}
	}

	// Step 3: Build Global SSA Supergraph
	fmt.Println(">> BUILDING GLOBAL SSA DATAFLOW GRAPH...")
	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm)
	prog.Build()

	fmt.Println(">> RUNNING IFDS TABULATION ENGINE...")
	LoopResolutions := Phase1_IFDS_Tabulation(initial, prog, visitor.LoopLocations, fset)
	
	Phase2_VerifyNPlusOne(LoopResolutions, visitor.LoopLocations)
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
// 2. IFDS PHASE 1: Dataflow Tracing (Global Project)
// ---------------------------------------------------------

func Phase1_IFDS_Tabulation(initial []*packages.Package, prog *ssa.Program, loopLocations map[string]string, fset *token.FileSet) map[string][]ssa.Value {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)
	LoopResolutions := make(map[string][]ssa.Value)

	allFunctions := getAllFunctions(initial, prog)

	// BASE CASE (GLOBAL SINK FINDER)
	for _, fn := range allFunctions {
		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				if call, ok := instr.(*ssa.Call); ok {

					methodName := getCallName(call)
					sinkIndex := getGormFetchArgIndex(methodName)

					if sinkIndex != -1 && len(call.Common().Args) > sinkIndex {
						val := call.Common().Args[sinkIndex]

						// BRIDGE 1: Trap direct loop fetch queries using "File:Line"
						pos := fset.Position(call.Pos())
						fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

						if loopLocations[fileLineKey] != "" {
							LoopResolutions[fileLineKey] = append(LoopResolutions[fileLineKey], val)
						}

						seed := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
						addPathEdge(&worklist, P_set, PathEdge{Start: seed, End: seed})
					}
				}
			}
		}
	}

	// WORKLIST TABULATION ALGORITHM
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

					// Return to all Callers globally!
					for _, caller := range getGlobalMockCallers(fn, allFunctions) {
						arg := caller.Common().Args[paramIdx]

						// BRIDGE 2: Global Cross-File Transitive checks
						pos := fset.Position(caller.Pos())
						fileLineKey := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)

						if loopLocations[fileLineKey] != "" {
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

func Phase2_VerifyNPlusOne(LoopResolutions map[string][]ssa.Value, LoopLocations map[string]string) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: GORM N+1 VULNERABILITY REPORT")
	fmt.Println("==========================================")

	if len(LoopResolutions) == 0 {
		fmt.Println(" ✅ Project Clean! No N+1 Fetch Queries detected in loops.")
		return
	}

	for locationKey, resolvedArgs := range LoopResolutions {
		funcName := LoopLocations[locationKey]

		isConstant := false
		var confirmedVal ssa.Value

		for _, arg := range resolvedArgs {
			if c, isConst := arg.(*ssa.Const); isConst {
				isConstant = true
				confirmedVal = c
			} else {
				confirmedVal = arg
			}
		}

		if isConstant {
			fmt.Printf(" ✅ [SAFE] \t%s \n\t-> Calls '%s()', but querying constants is safe: %s\n\n", locationKey, funcName, confirmedVal.String())
		} else {
			fmt.Printf(" 🚨 [TRUE N+1] \t%s \n\t-> Loop dynamically fetches using loop variable %s\n\n", locationKey, formatVal(confirmedVal))
		}
	}
}

// ============================================================================
// GLOBAL PROJECT UTILITIES
// ============================================================================

func getAllFunctions(initial []*packages.Package, prog *ssa.Program) []*ssa.Function {
	var funcs []*ssa.Function

	// 1. Identify which packages are actually part of the user's target project
	userPackages := make(map[*types.Package]bool)
	for _, p := range initial {
		if p.Types != nil {
			userPackages[p.Types] = true
		}
	}

	// 2. Only extract functions from the user's packages
	for _, pkg := range prog.AllPackages() {
		if pkg == nil || pkg.Pkg == nil {
			continue
		}

		// SKIP standard library and third-party dependencies!
		if !userPackages[pkg.Pkg] {
			continue
		}

		for _, mem := range pkg.Members {
			// Get standard functions
			if fn, ok := mem.(*ssa.Function); ok {
				funcs = append(funcs, fn)
			}
			// Get methods attached to types
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

// Searches ALL packages for call sites of a specific function
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

// ============================================================================
// GORM SINK DETECTION & IFDS MATH LOGIC
// ============================================================================

func getCallName(call *ssa.Call) string {
	if call.Call.Method != nil {
		return call.Call.Method.Name()
	}
	if callee := call.Call.StaticCallee(); callee != nil {
		return callee.Name()
	}
	return ""
}

func getGormFetchArgIndex(methodName string) int {
	switch methodName {
	case "Where", "Raw", "Not", "Or", "Select", "Find", "First", "Last", "Take", "Scan", "Pluck":
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

func formatVal(v ssa.Value) string {
	if v.Name() != "" {
		return "'" + v.Name() + "'"
	}
	return fmt.Sprintf("%T", v)
}
