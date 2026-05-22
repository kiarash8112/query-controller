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

const targetCode = `
package main

type DB struct{}
func (db *DB) Query(q string) {}

func fetchTarget(target string) {
    db := &DB{}
    db.Query(target) // THE SINK
}

func main() {
    users := []string{"admin", "guest"}
    db := &DB{}

    // Scenario 1: Direct N+1 (True Positive)
    for _, u := range users {
        db.Query(u)
    }

    // Scenario 2: Transitive N+1 (True Positive)
    for _, u := range users {
        fetchTarget(u)
    }

    // Scenario 3: Transitive N+1 (FALSE POSITIVE!)
    for _, _ = range users {
        fetchTarget("SELECT * FROM static_table")
    }
}
`

// ---------------------------------------------------------
// 1. AST PHASE: Loop Context Detection (Fixed to use Line Number)
// ---------------------------------------------------------

type ASTLoopVisitor struct {
	fset      *token.FileSet
	inLoop    bool
	LoopLines map[int]string // Line Number -> Called Function Name
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

			// Extract Line Number to create a perfect bridge to SSA
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

	fmt.Println(">> STARTING AST + IFDS MERGED N+1 ANALYSIS...")

	// Pass fset into IFDS so it can look up line numbers!
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
// 2. IFDS PHASE 1: Dataflow Tracing
// ---------------------------------------------------------

func Phase1_IFDS_Tabulation(ssaPkg *ssa.Package, loopLines map[int]string, fset *token.FileSet) map[int][]ssa.Value {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)

	// Map resolved IFDS data facts strictly to Line Numbers!
	LoopResolutions := make(map[int][]ssa.Value)

	// BASE CASE
	for _, mem := range ssaPkg.Members {
		if fn, ok := mem.(*ssa.Function); ok {
			for _, block := range fn.Blocks {
				for i, instr := range block.Instrs {
					if call, ok := instr.(*ssa.Call); ok && call.Common().Value.Name() == "Query" {
						val := call.Common().Args[1]

						// BRIDGE 1: Line number check
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

						// BRIDGE 2: Line number check when exiting Transitive loops!
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
			fmt.Printf(" ✅ [FALSE POSITIVE SAFELY ELIMINATED] Line %d: Loop calls '%s', but query is static: %s\n", line, funcName, confirmedVal.String())
		} else {
			fmt.Printf(" 🚨 [TRUE N+1 VULNERABILITY DETECTED]  Line %d: Loop dynamically queries using %s\n", line, formatVal(confirmedVal))
		}
	}
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
