package linters

import (
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

type ExplodedNode struct {
	Instr ssa.Instruction
	Fact  ssa.Value
}

type PathEdge struct {
	Start ExplodedNode
	End   ExplodedNode
}

type IFDSTracer struct {
	fn       *ssa.Function
	localRet map[*ssa.Function]*ReturnToParamFact
	pass     *analysis.Pass

	P_set    map[PathEdge]bool
	worklist []PathEdge
}

func NewIFDSTracer(fn *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *IFDSTracer {
	return &IFDSTracer{
		fn:       fn,
		localRet: localRet,
		pass:     pass,
		P_set:    make(map[PathEdge]bool),
		worklist: make([]PathEdge, 0),
	}
}

func (t *IFDSTracer) addPathEdge(edge PathEdge) {
	if !t.P_set[edge] {
		t.P_set[edge] = true
		t.worklist = append(t.worklist, edge)
	}
}

// Tabulate runs the formal Worklist algorithm using PathEdges
func (t *IFDSTracer) Tabulate(startInstr ssa.Instruction, startFact ssa.Value, loopRanges []LoopRange) ([]int, bool) {
	startNode := ExplodedNode{Instr: startInstr, Fact: startFact}
	t.addPathEdge(PathEdge{Start: startNode, End: startNode})

	hitLoop := false
	paramsReached := make(map[int]bool)

	for len(t.worklist) > 0 {
		edge := t.worklist[0]
		t.worklist = t.worklist[1:]

		v2 := edge.End.Instr
		d2 := edge.End.Fact

		if d2 == nil {
			continue
		}

		// 1. Target Conditions
		if isLoopIterator(d2, loopRanges) {
			hitLoop = true
			continue // Path terminates at vulnerability
		}
		if param, ok := d2.(*ssa.Parameter); ok {
			if idx := indexOfParam(t.fn, param); idx >= 0 {
				paramsReached[idx] = true
			}
			continue // Path terminates at function boundary
		}
		if _, isConst := d2.(*ssa.Const); isConst {
			continue // Path terminates safely
		}

		// 2. Data Flow Resolutions
		if call, ok := d2.(*ssa.Call); ok {

			// --- CROSS-PACKAGE SUMMARY JUMP ---
			// E.g., `id := pk1.getuserID(user)`
			// We check if `pk1.getuserID` has an exported ReturnFact
			retFact := t.getReturnFact(call.Call.StaticCallee())

			if retFact != nil {
				// We drop `id` and instantly create a PathEdge for `user` based on the Summary map
				for _, mappedParams := range retFact.ResultToParams {
					for _, pIdx := range mappedParams {
						if pIdx < len(call.Call.Args) {
							argFact := call.Call.Args[pIdx]
							t.addPathEdge(PathEdge{
								Start: edge.Start,
								End:   ExplodedNode{Instr: call, Fact: argFact},
							})
						}
					}
				}
				continue
			}
		}

		// 3. Standard Instruction Flow
		predecessors := getPredecessorFacts(d2)
		for _, pFact := range predecessors {
			var pInstr ssa.Instruction
			if instr, isInstr := pFact.(ssa.Instruction); isInstr {
				pInstr = instr // The predecessor value is an instruction itself
			} else {
				pInstr = v2 // Fallback to current context
			}

			t.addPathEdge(PathEdge{
				Start: edge.Start,
				End:   ExplodedNode{Instr: pInstr, Fact: pFact},
			})
		}
	}

	var res []int
	for p := range paramsReached {
		res = append(res, p)
	}
	return res, hitLoop
}

func (t *IFDSTracer) getReturnFact(callee *ssa.Function) *ReturnToParamFact {
	if callee == nil {
		return nil
	}
	if fr, ok := t.localRet[callee]; ok {
		return fr
	}
	if obj := callee.Object(); obj != nil {
		var exportedFact ReturnToParamFact
		if t.pass.ImportObjectFact(obj, &exportedFact) {
			return &exportedFact
		}
	}
	return nil
}
