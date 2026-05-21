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

// The target program. Notice we call buildQuery TWICE.
// The algorithm will trace q1 completely, generate a Summary Edge,
// and when it tracks q2, it will use the summary and bypass the function!
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
    
    // Call Site 1
    q1 := buildQuery("users")
    db.Query(q1) // SINK 1

    // Call Site 2
    q2 := buildQuery("admins")
    db.Query(q2) // SINK 2
}
`

// ProgramPoint (n): A location in the Supergraph.
type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}

// PathEdge <d1, n, d2>: Tracks a fact (d2) through a function,
// remembering the context (d1) of how we entered the function backwards.
type PathEdge struct {
	ContextFact ssa.Value    // d1 (Fact at the function's boundary)
	Point       ProgramPoint // n  (Current location)
	CurrentFact ssa.Value    // d2 (Fact right now)
}

// SummaryCache: <Function -> <d1 -> []d2>>
// Automatically maps a tracked return value to the parameters that caused it.
type SummaryCache map[*ssa.Function]map[ssa.Value][]ssa.Value

func main() {
	// 1. Setup and Build Go SSA
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "main.go", targetCode, 0)
	pkg := types.NewPackage("main", "")
	ssaPkg, _, _ := ssautil.BuildPackage(&types.Config{Importer: nil}, fset, pkg, []*ast.File{f}, ssa.SanityCheckFunctions)
	ssaPkg.Build()

	var worklist []PathEdge
	visited := make(map[PathEdge]bool)
	summaries := make(SummaryCache)

	// 2. Find all Sinks and Push Initial PathEdges
	mainFunc := ssaPkg.Func("main")
	for _, block := range mainFunc.Blocks {
		for i, instr := range block.Instrs {
			if call, ok := instr.(*ssa.Call); ok && call.Common().Value.Name() == "Query" {
				// We track the 2nd argument (the query string). Index 1.
				fact := call.Common().Args[1]
				worklist = append(worklist, PathEdge{
					ContextFact: fact,
					Point:       ProgramPoint{Block: block, Index: i},
					CurrentFact: fact,
				})
				fmt.Printf(">> Found Sink! Initializing track for: %s\n", formatVal(fact))
			}
		}
	}

	fmt.Println("\n>> Starting IFDS Tabulation Traversal (Reverse)...")

	// 3. IFDS Tabulation Worklist Solver
	for len(worklist) > 0 {
		edge := worklist[0]
		worklist = worklist[1:]

		if visited[edge] {
			continue
		}
		visited[edge] = true

		// If we hit a hardcoded string, print the taint source and stop tracking this path
		if _, isConst := edge.CurrentFact.(*ssa.Const); isConst {
			fmt.Printf("\n[Source Found] Value traced back to constant: %s\n", edge.CurrentFact.String())
			continue
		}

		instr := edge.Point.Block.Instrs[edge.Point.Index]

		// --- A. CALL INSTRUCTION FLOW ---
		if callInstr, ok := instr.(ssa.CallInstruction); ok {

			// 1. CallToReturn Flow (Bypassing unaffected local facts)
			for _, bypassFact := range CallToReturnFlow(callInstr, edge.CurrentFact) {
				for _, prevPoint := range getPredecessors(edge.Point) {
					worklist = append(worklist, PathEdge{
						ContextFact: edge.ContextFact,
						Point:       prevPoint,
						CurrentFact: bypassFact,
					})
				}
			}

			// 2. CallFlow (Entering Callee Backwards)
			if callVal, ok := callInstr.(ssa.Value); ok && callVal == edge.CurrentFact {
				callee := callInstr.Common().StaticCallee()
				if callee != nil {
					// === IFDS MAGIC: CHECK SUMMARY CACHE ===
					if cacheFunc, ok := summaries[callee]; ok {
						if cachedResults, exists := cacheFunc[edge.CurrentFact]; exists {
							fmt.Printf("\n[Summary Hit!] Bypassing '%s'. Transforming %s instantly.\n", callee.Name(), formatVal(edge.CurrentFact))

							for _, cachedD2 := range cachedResults {
								// 1. cachedD2 is the parameter we tracked back to. Find its index.
								paramIndex := -1
								for i, param := range callee.Params {
									if param == cachedD2 {
										paramIndex = i
										break
									}
								}

								// 2. Map that parameter backward to the argument passed by the caller
								if paramIndex != -1 {
									arg := callInstr.Common().Args[paramIndex]
									for _, prevPoint := range getPredecessors(edge.Point) {
										worklist = append(worklist, PathEdge{
											ContextFact: edge.ContextFact,
											Point:       prevPoint,
											CurrentFact: arg, // Using the correctly mapped argument!
										})
									}
								}
							}
							continue // STOP evaluating this function. We used the summary!
						}
					}

					// === IF SUMMARY MISSES: EXPLORE CALLEE (CallFlow) ===
					fmt.Printf("[CallFlow] Diving backward into %s...\n", callee.Name())
					for _, block := range callee.Blocks {
						for i, retInstr := range block.Instrs {
							if ret, ok := retInstr.(*ssa.Return); ok {
								// d1 (Context) safely becomes the returned value
								newContext := ret.Results[0]
								worklist = append(worklist, PathEdge{
									ContextFact: newContext,
									Point:       ProgramPoint{Block: block, Index: i},
									CurrentFact: newContext,
								})
							}
						}
					}
				}
			}

			// --- B. ENTRY NODE FLOW (EXITING CALLEE) ---
		} else if isEntryNode(edge.Point) {
			fn := edge.Point.Block.Parent()

			// Try to map the current fact to a function parameter
			paramIndex := -1
			for i, param := range fn.Params {
				if param == edge.CurrentFact {
					paramIndex = i
					break
				}
			}

			if paramIndex != -1 {
				// === IFDS MAGIC: CREATE SUMMARY EDGE (d1 -> d2) ===
				if summaries[fn] == nil {
					summaries[fn] = make(map[ssa.Value][]ssa.Value)
				}
				summaries[fn][edge.ContextFact] = append(summaries[fn][edge.ContextFact], edge.CurrentFact)
				fmt.Printf("[Summary Created] Function '%s': Mapping <Ret: %s> -> <Param Index: %d>\n", fn.Name(), formatVal(edge.ContextFact), paramIndex)

				// Push Fact back to all callers (Mock Callgraph ReturnFlow)
				for _, caller := range getMockCallers(fn) {
					callerPoint := getInstructionPoint(caller)
					for _, prevPoint := range getPredecessors(callerPoint) {
						arg := caller.Common().Args[paramIndex]
						worklist = append(worklist, PathEdge{
							ContextFact: arg, // Resetting context to the caller's scope
							Point:       prevPoint,
							CurrentFact: arg,
						})
					}
				}
			}

			// --- C. NORMAL INSTRUCTION FLOW ---
		} else {
			newFacts := NormalFlow(instr, edge.CurrentFact)
			for _, prevPoint := range getPredecessors(edge.Point) {
				for _, nf := range newFacts {
					worklist = append(worklist, PathEdge{
						ContextFact: edge.ContextFact, // Context passes through
						Point:       prevPoint,
						CurrentFact: nf,
					})
				}
			}
		}
	}
}

// ==========================================
// TRANSFER FUNCTIONS AND UTILS
// ==========================================

// NormalFlow handles assignments and operations (Kill/Gen logic)
func NormalFlow(instr ssa.Instruction, fact ssa.Value) []ssa.Value {
	if instrVal, ok := instr.(ssa.Value); ok && instrVal == fact {
		var newFacts []ssa.Value
		for _, opPtr := range instr.Operands(nil) {
			if opPtr != nil && *opPtr != nil {
				newFacts = append(newFacts, *opPtr)
			}
		}
		return newFacts
	} else if store, ok := instr.(*ssa.Store); ok && store.Addr == fact {
		return []ssa.Value{store.Val} // Tracing backwards through memory store
	}
	return []ssa.Value{fact}
}

// CallToReturnFlow bypasses local variables safely over function calls
func CallToReturnFlow(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		return nil // Value was created here, it gets killed for the bypass
	}
	return []ssa.Value{fact} // Fact is untouched by the function, passes through
}

// isEntryNode determines if we have hit the top of a function while traversing backwards
func isEntryNode(node ProgramPoint) bool {
	return node.Index == 0 && node.Block.Index == 0
}

// getPredecessors resolves standard CFG edges reversed
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

// formatVal returns a pretty string for a variable
func formatVal(v ssa.Value) string {
	if v.Name() != "" {
		return fmt.Sprintf("'%s'", v.Name())
	}
	return fmt.Sprintf("%T", v)
}

// getMockCallers looks through the whole package to find where a function is called.
// This simulates pulling CallGraph edges dynamically.
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
