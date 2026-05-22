package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// Target code demonstrating GORM Fetch queries vs GORM Insert queries
const targetCode = `
package main

type GormDB struct{}
func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB { return db }
func (db *GormDB) Create(value interface{}) *GormDB { return db }

func fetchTarget(target string) {
    db := &GormDB{}
    db.Where(target).Find(nil) // GORM FETCH SINK
}

func insertTarget(target string) {
    db := &GormDB{}
    db.Create(target) // GORM INSERT SINK
}

func main() {
    users := []string{"admin", "guest"}
    db := &GormDB{}

    // Scenario 1: Fetch N+1 (TRUE POSITIVE)
    for _, u := range users {
        db.Where(u).Find(nil)
    }

    // Scenario 2: Transitive Fetch N+1 (TRUE POSITIVE)
    for _, u := range users {
        fetchTarget(u)
    }

    // Scenario 3: Transitive False Positive (STATIC FETCH)
    for _, _ = range users {
        fetchTarget("SELECT * FROM static_table")
    }

    // Scenario 4: Transitive Insert (IGNORED! Not a fetch)
    for _, u := range users {
        insertTarget(u)
    }
    
    // Scenario 5: Direct Insert (IGNORED! Not a fetch)
    for _, u := range users {
        db.Create(u)
    }
}
`

// ---------------------------------------------------------
// 1. AST PHASE: Loop Context Detection
// ---------------------------------------------------------

type ASTLoopVisitor struct {
	fset      *token.FileSet
	inLoop    bool
	LoopLines map[int]string
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
			line := v.fset.Position(call.Pos()).Line
			v.LoopLines[line] = name
		}
	}
	return &ASTLoopVisitor{fset: v.fset, inLoop: isLoop, LoopLines: v.LoopLines}
}

// ---------------------------------------------------------
// MAIN EXECUTION
// ---------------------------------------------------------

func main() {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "main.go", targetCode, 0)

	visitor := &ASTLoopVisitor{fset: fset, LoopLines: make(map[int]string)}
	ast.Walk(visitor, f)

	pkg := types.NewPackage("main", "")
	ssaPkg, _, _ := ssautil.BuildPackage(&types.Config{Importer: nil}, fset, pkg, []*ast.File{f}, ssa.SanityCheckFunctions|ssa.NaiveForm)
	ssaPkg.Build()

	fmt.Println(">> STARTING AST + IFDS MERGED N+1 ANALYSIS (GORM FETCH ONLY)...")

	LoopResolutions := Phase1_IFDS_Tabulation(ssaPkg, visitor.LoopLines, fset)
	Phase2_VerifyNPlusOne(LoopResolutions, visitor.LoopLines)
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
// 2. IFDS PHASE 1: Dataflow Tracing (Gorm Support)
// ---------------------------------------------------------

func Phase1_IFDS_Tabulation(ssaPkg *ssa.Package, loopLines map[int]string, fset *token.FileSet) map[int][]ssa.Value {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)
	LoopResolutions := make(map[int][]ssa.Value)

	// BASE CASE (SINK FINDER)
	for _, mem := range ssaPkg.Members {
		if fn, ok := mem.(*ssa.Function); ok {
			for _, block := range fn.Blocks {
				for i, instr := range block.Instrs {
					if call, ok := instr.(*ssa.Call); ok {

						// NEW: Check if the method is a GORM Fetch method!
						methodName := getCallName(call)
						sinkIndex := getGormFetchArgIndex(methodName)

						if sinkIndex != -1 && len(call.Common().Args) > sinkIndex {
							val := call.Common().Args[sinkIndex]

							// BRIDGE 1: Trap direct loop fetch queries
							line := fset.Position(call.Pos()).Line
							if loopLines[line] != "" {
								LoopResolutions[line] = append(LoopResolutions[line], val)
							}

							seed := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
							addPathEdge(&worklist, P_set, PathEdge{Start: seed, End: seed})
						}
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

					for _, caller := range getMockCallers(fn) {
						arg := caller.Common().Args[paramIdx]

						// BRIDGE 2: Trap Wrapper loops hitting GORM Fetch methods
						callerLine := fset.Position(caller.Pos()).Line
						if loopLines[callerLine] != "" {
							LoopResolutions[callerLine] = append(LoopResolutions[callerLine], arg)
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

func Phase2_VerifyNPlusOne(LoopResolutions map[int][]ssa.Value, LoopLines map[int]string) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: IFDS FALSE POSITIVE ELIMINATION")
	fmt.Println("==========================================")

	for line, resolvedArgs := range LoopResolutions {
		funcName := LoopLines[line]

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
			fmt.Printf(" ✅ [SAFE] Line %d: Loop calls '%s', but GORM fetch is static: %s\n", line, funcName, confirmedVal.String())
		} else {
			fmt.Printf(" 🚨 [TRUE N+1 FETCH] Line %d: Loop dynamically fetches using %s\n", line, formatVal(confirmedVal))
		}
	}
}

// ============================================================================
// GORM SINK DETECTION
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

// Returns the index of the queried variable for Gorm FETCH methods
func getGormFetchArgIndex(methodName string) int {
	switch methodName {
	// EXPLICIT WHITELIST: Fetch APIs only.
	// Operations like Create, Save, Update, Delete will return -1 and be ignored!
	case "Where", "Raw", "Not", "Or", "Select", "Find", "First", "Last", "Take", "Scan", "Pluck":
		return 1 // Arg 0 is the receiver string (*GormDB). Arg 1 is the string/variable!
	case "Query", "QueryRow", "Exec":
		return 1 // Retained standard library support
	}
	return -1
}

// ============================================================================
// LOGIC UTILITIES
// ============================================================================

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

func getMockCallers(fn *ssa.Function) []ssa.CallInstruction {
	var callers []ssa.CallInstruction
	for _, memb := range fn.Package().Members {
		if cFn, ok := memb.(*ssa.Function); ok {
			for _, b := range cFn.Blocks {
				for _, inst := range b.Instrs {
					if callInst, ok := inst.(ssa.CallInstruction); ok && callInst.Common().StaticCallee() == fn {
						callers = append(callers, callInst)
					}
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

func formatVal(v ssa.Value) string {
	if v.Name() != "" {
		return "'" + v.Name() + "'"
	}
	return fmt.Sprintf("%T", v)
}
