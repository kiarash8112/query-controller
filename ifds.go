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

func buildQuery(table string) string {
    res := "SELECT * FROM " + table
    return res
}

func main() {
    db := &DB{}
    
    q1 := buildQuery("users") // Call 1
    db.Query(q1)              // Sink 1

    q2 := buildQuery("admins")// Call 2 (Should use Summary Cache!)
    db.Query(q2)              // Sink 2
}
`

// ProgramPoint (v): A location in the CFG.
type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}

// ExplodedNode ⟨v, d⟩: Pairs a CFG node with a Dataflow Fact.
type ExplodedNode struct {
	Point ProgramPoint // The v
	Fact  ssa.Value    // The d
}

// PathEdge ⟨v1, d1⟩ ⇝ ⟨v2, d2⟩: Connects the function entry context to the current state.
// (Note: In reverse analysis, v1 is the Return site, and it walks backwards to the Entry site).
type PathEdge struct {
	Start ExplodedNode // ⟨v1, d1⟩
	End   ExplodedNode // ⟨v2, d2⟩
}

// SummaryCache: Maps Function -> (StartFact d1 -> EndFact d2)
// When a PathEdge safely crosses an entire function, it creates a Summary.
type SummaryCache map[*ssa.Function]map[ssa.Value][]ssa.Value

func main() {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "main.go", targetCode, 0)
	pkg := types.NewPackage("main", "")
	ssaPkg, _, _ := ssautil.BuildPackage(&types.Config{Importer: nil}, fset, pkg, []*ast.File{f}, ssa.SanityCheckFunctions)
	ssaPkg.Build()

	// The 'P' set from the book: The set of discovered PathEdges
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	summaries := make(SummaryCache)

	// 1. Initialize P set with the Sinks
	mainFunc := ssaPkg.Func("main")
	for _, block := range mainFunc.Blocks {
		for i, instr := range block.Instrs {
			if call, ok := instr.(*ssa.Call); ok && call.Common().Value.Name() == "Query" {
				val := call.Common().Args[1]

				// ⟨v1, d1⟩
				startContext := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}

				// Initial edge: ⟨v1, d1⟩ ⇝ ⟨v1, d1⟩
				initialEdge := PathEdge{Start: startContext, End: startContext}

				worklist = append(worklist, initialEdge)
				P_set[initialEdge] = true
			}
		}
	}

	fmt.Println(">> Starting Mathematically Strict IFDS...")

	// 2. Build the P set (Worklist Solver)
	for len(worklist) > 0 {
		edge := worklist[0]
		worklist = worklist[1:]

		// We extract the Current State: ⟨v2, d2⟩
		v2 := edge.End.Point
		d2 := edge.End.Fact

		if _, isConst := d2.(*ssa.Const); isConst {
			fmt.Printf("\n[Source Found] Constant string: %s\n", d2.String())
			continue
		}

		instr := v2.Block.Instrs[v2.Index]

		if callInstr, ok := instr.(ssa.CallInstruction); ok {

			// --- CALL FLOW (Diving completely into a new function) ---
			if callVal, ok := callInstr.(ssa.Value); ok && callVal == d2 {
				callee := callInstr.Common().StaticCallee()
				if callee != nil {

					// SUMMARY LOOKUP
					if cache, ok := summaries[callee]; ok {
						if cachedResults, exists := cache[d2]; exists {
							fmt.Printf("[Summary Hit!] Using cached traversal for '%s'.\n", callee.Name())
							for _, paramD2 := range cachedResults {
								paramIndex := getParamIndex(callee, paramD2)
								if paramIndex != -1 {
									arg := callInstr.Common().Args[paramIndex]
									for _, prevPoint := range getPredecessors(v2) {
										pushToPathEdges(&worklist, P_set, PathEdge{
											Start: edge.Start, // Original context preserved
											End:   ExplodedNode{Point: prevPoint, Fact: arg},
										})
									}
								}
							}
							continue
						}
					}

					// NO SUMMARY: Create a completely NEW Context Boundary ⟨v1_new, d1_new⟩
					fmt.Printf("[CallFlow] Creating new PathEdge analysis inside '%s'\n", callee.Name())
					for _, block := range callee.Blocks {
						for i, retInstr := range block.Instrs {
							if ret, ok := retInstr.(*ssa.Return); ok {
								// ⟨v1_new, d1_new⟩
								newStartContext := ExplodedNode{
									Point: ProgramPoint{Block: block, Index: i},
									Fact:  ret.Results[0],
								}
								// ⟨v1_new, d1_new⟩ ⇝ ⟨v1_new, d1_new⟩
								pushToPathEdges(&worklist, P_set, PathEdge{
									Start: newStartContext,
									End:   newStartContext,
								})
							}
						}
					}
				}
			} else {
				// CallToReturn Flow (Bypass local variables)
				for _, prevPoint := range getPredecessors(v2) {
					pushToPathEdges(&worklist, P_set, PathEdge{
						Start: edge.Start,
						End:   ExplodedNode{Point: prevPoint, Fact: d2},
					})
				}
			}

		} else if isEntryNode(v2) {
			// --- RETURN FLOW (Reached function boundary) ---
			fn := v2.Block.Parent()
			paramIndex := getParamIndex(fn, d2)

			if paramIndex != -1 {
				// We reached the opposite boundary! The PathEdge is complete.
				// We extract d1 from Start Fact, and d2 from End Fact to build the Summary Edge.
				d1 := edge.Start.Fact

				if summaries[fn] == nil {
					summaries[fn] = make(map[ssa.Value][]ssa.Value)
				}
				summaries[fn][d1] = append(summaries[fn][d1], d2)
				fmt.Printf("[Summary Added] ⟨%s⟩ ⇝ ⟨%s⟩ recorded for '%s'\n", formatVal(d1), formatVal(d2), fn.Name())

				// Map the parameter back up to all Callers
				for _, caller := range getMockCallers(fn) {
					callerPoint := getInstructionPoint(caller)
					arg := caller.Common().Args[paramIndex]
					for _, prevPoint := range getPredecessors(callerPoint) {
						// Crucial: Notice how the Context (Start) resets to whatever it was BEFORE the caller called it.
						// Since we fake the Callgraph Return here, we seed a new "continuation" in the Caller.
						pushToPathEdges(&worklist, P_set, PathEdge{
							Start: ExplodedNode{Point: prevPoint, Fact: arg}, // Reset context to caller scope
							End:   ExplodedNode{Point: prevPoint, Fact: arg},
						})
					}
				}
			}

		} else {
			// --- NORMAL FLOW (Move v2, d2 incrementally) ---
			newFacts := NormalFlow(instr, d2)
			for _, prevPoint := range getPredecessors(v2) {
				for _, nd2 := range newFacts {
					pushToPathEdges(&worklist, P_set, PathEdge{
						Start: edge.Start,                                // v1, d1 stays exactly the same
						End:   ExplodedNode{Point: prevPoint, Fact: nd2}, // v2, d2 updates
					})
				}
			}
		}
	}
}

// Safely adds a PathEdge to the P Set if it hasn't been discovered yet
func pushToPathEdges(worklist *[]PathEdge, pSet map[PathEdge]bool, edge PathEdge) {
	if !pSet[edge] {
		pSet[edge] = true
		*worklist = append(*worklist, edge)
	}
}

// (Other utility functions remain the same: NormalFlow, getPredecessors, getMockCallers, formatVal, etc)

func NormalFlow(instr ssa.Instruction, fact ssa.Value) []ssa.Value {
	if instrVal, ok := instr.(ssa.Value); ok && instrVal == fact {
		var newFacts []ssa.Value
		for _, opPtr := range instr.Operands(nil) {
			if opPtr != nil && *opPtr != nil {
				newFacts = append(newFacts, *opPtr) // Kill / Gen
			}
		}
		return newFacts
	} else if store, ok := instr.(*ssa.Store); ok && store.Addr == fact {
		return []ssa.Value{store.Val}
	}
	return []ssa.Value{fact} // Flow through unmodified
}

func getParamIndex(fn *ssa.Function, fact ssa.Value) int {
	for i, param := range fn.Params {
		if param == fact {
			return i
		}
	}
	return -1
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

func formatVal(v ssa.Value) string {
	if v.Name() != "" {
		return v.Name()
	}
	return fmt.Sprintf("%T", v)
}

func getMockCallers(fn *ssa.Function) []ssa.CallInstruction {
	var callers []ssa.CallInstruction
	for _, member := range fn.Package().Members {
		if callerFn, ok := member.(*ssa.Function); ok {
			for _, b := range callerFn.Blocks {
				for _, inst := range b.Instrs {
					if callInst, ok := inst.(ssa.CallInstruction); ok {
						if callInst.Common().StaticCallee() == fn {
							callers = append(callers, callInst)
						}
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
