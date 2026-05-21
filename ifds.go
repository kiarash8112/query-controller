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
    userInput := "users"
    
    q := buildQuery(userInput)

    // SINK
    db.Query(q)
}
`

// ProgramPoint (n): A location in the Supergraph.
type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}

// ExplodedNode (n, d): A pair representing a node in the Exploded Supergraph.
type ExplodedNode struct {
	Point ProgramPoint
	Fact  ssa.Value // The Dataflow Fact (d)
}

func main() {
	// 1. Build SSA Representation
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", targetCode, 0)
	if err != nil {
		panic(err)
	}

	pkg := types.NewPackage("main", "")
	ssaPkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: nil}, fset, pkg, []*ast.File{f}, ssa.SanityCheckFunctions,
	)
	if err != nil {
		panic(err)
	}
	ssaPkg.Build()

	// 2. Initialize Worklist (Graph Reachability Algorithm)
	var worklist []ExplodedNode
	visited := make(map[ExplodedNode]bool)

	// Find the Sink and create the initial ExplodedNode (Start Node)
	mainFunc := ssaPkg.Func("main")
	for _, block := range mainFunc.Blocks {
		for i, instr := range block.Instrs {
			if call, ok := instr.(*ssa.Call); ok && call.Common().Value.Name() == "Query" {
				fmt.Println(">> Found Sink! Starting Reachability on Exploded Supergraph...")
				startNode := ExplodedNode{
					Point: ProgramPoint{Block: block, Index: i},
					Fact:  call.Common().Args[1],
				}
				worklist = append(worklist, startNode)
			}
		}
	}

	// 3. IFDS Worklist Algorithm (Exploded Supergraph Traversal)
	for len(worklist) > 0 {
		node := worklist[0]
		worklist = worklist[1:]

		if visited[node] {
			continue
		}
		visited[node] = true

		if _, isConst := node.Fact.(*ssa.Const); isConst {
			fmt.Printf("[Source Found] Hardcoded Trace End: %s\n", node.Fact.String())
			continue
		}

		instr := node.Point.Block.Instrs[node.Point.Index]
		var nextNodes []ExplodedNode

		// --- TRANSFER FUNCTION DEMULTIPLEXER ---
		if callInstr, ok := instr.(ssa.CallInstruction); ok {

			// Transfer 1: CallToReturn Flow (Bypass into previous instruction in Caller)
			bypassFacts := CallToReturnFlow(callInstr, node.Fact)
			for _, prevPoint := range getPredecessors(node.Point) {
				for _, bf := range bypassFacts {
					nextNodes = append(nextNodes, ExplodedNode{Point: prevPoint, Fact: bf})
				}
			}

			// Transfer 2: Call Flow (Jump backwards into Callee's return statements)
			nextNodes = append(nextNodes, CallFlow(callInstr, node.Fact)...)

		} else if isEntryNode(node.Point) {

			// Transfer 3: Return Flow (Jump backwards out of Callee to Caller's call site)
			nextNodes = append(nextNodes, ReturnFlow(node.Point, node.Fact)...)

		} else {

			// Transfer 4: Normal Flow (Standard instruction Kill/Gen)
			newFacts := NormalFlow(instr, node.Fact)
			for _, prevPoint := range getPredecessors(node.Point) {
				for _, nf := range newFacts {
					nextNodes = append(nextNodes, ExplodedNode{Point: prevPoint, Fact: nf})
				}
			}
		}

		// Add newly discovered exploded nodes to the worklist
		worklist = append(worklist, nextNodes...)
	}
}

// ==========================================
// MATHEMATICAL TRANSFER FUNCTIONS (REVERSE)
// ==========================================

// 1. NormalFlow: D -> 2^D (Standard Statement Transformer)
func NormalFlow(instr ssa.Instruction, fact ssa.Value) []ssa.Value {
	if instrVal, ok := instr.(ssa.Value); ok && instrVal == fact {
		// Fact killed. Operands generated.
		var newFacts []ssa.Value
		for _, opPtr := range instr.Operands(nil) {
			if opPtr != nil && *opPtr != nil {
				newFacts = append(newFacts, *opPtr)
			}
		}
		return newFacts
	} else if store, ok := instr.(*ssa.Store); ok && store.Addr == fact {
		return []ssa.Value{store.Val}
	}
	return []ssa.Value{fact}
}

// 2. CallToReturnFlow: D -> 2^D (Bypass un-affected Facts around methods)
func CallToReturnFlow(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
	// If the fact was the return value of this exact call, we MUST enter the function. Kill it here.
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		return nil
	}
	// Pass fact around the function safely.
	return []ssa.Value{fact}
}

// 3. CallFlow: (ProgramPoint, Fact) -> {ExplodedNodes} (Caller -> Callee)
func CallFlow(call ssa.CallInstruction, fact ssa.Value) []ExplodedNode {
	var targetNodes []ExplodedNode
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		callee := call.Common().StaticCallee()
		if callee == nil {
			return nil
		}

		fmt.Printf("[Transfer] (CallFlow) Shifting graph focus down into %s's return statements\n", callee.Name())

		for _, block := range callee.Blocks {
			for i, instr := range block.Instrs {
				if ret, ok := instr.(*ssa.Return); ok {
					targetNodes = append(targetNodes, ExplodedNode{
						Point: ProgramPoint{Block: block, Index: i},
						Fact:  ret.Results[0],
					})
				}
			}
		}
	}
	return targetNodes
}

// 4. ReturnFlow: (ProgramPoint, Fact) -> {ExplodedNodes} (Callee -> Caller)
func ReturnFlow(entryPoint ProgramPoint, fact ssa.Value) []ExplodedNode {
	fn := entryPoint.Block.Parent()

	// Identify if tracked fact mathematically maps to a parameter
	paramIndex := -1
	for i, param := range fn.Params {
		if param == fact {
			paramIndex = i
			break
		}
	}
	if paramIndex == -1 {
		return nil // Fact dies mathematically (no interprocedural path)
	}

	fmt.Printf("[Transfer] (ReturnFlow) Shifting graph focus backward out of %s to its call sites\n", fn.Name())

	// Mock CallGraph Edge Resolution
	var targetNodes []ExplodedNode
	mainFunc := fn.Prog.FuncValue(fn.Pkg.Pkg.Scope().Lookup("main").(*types.Func))
	for _, b := range mainFunc.Blocks {
		for i, instr := range b.Instrs {
			if call, ok := instr.(ssa.CallInstruction); ok && call.Common().StaticCallee() == fn {
				// Jump to the ExplodedNode representing the exact Caller Argument BEFORE the call
				targetNodes = append(targetNodes, ExplodedNode{
					Point: getPredecessors(ProgramPoint{Block: b, Index: i})[0], // Simplified extraction
					Fact:  call.Common().Args[paramIndex],
				})
			}
		}
	}

	return targetNodes
}

// ==========================================
// GRAPH TRAVERSAL UTILS (SUPERGRAPH EDGES)
// ==========================================

func isEntryNode(node ProgramPoint) bool {
	return node.Index == 0 && node.Block.Index == 0
}

func getPredecessors(p ProgramPoint) []ProgramPoint {
	// Standard intra-block graph edge
	if p.Index > 0 {
		return []ProgramPoint{{Block: p.Block, Index: p.Index - 1}}
	}
	// Control-Flow block jump (crossing CFG edge reversed)
	var preds []ProgramPoint
	for _, predBlock := range p.Block.Preds {
		if len(predBlock.Instrs) > 0 {
			preds = append(preds, ProgramPoint{Block: predBlock, Index: len(predBlock.Instrs) - 1})
		}
	}
	return preds
}
