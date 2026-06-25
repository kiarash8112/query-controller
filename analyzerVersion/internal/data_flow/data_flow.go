package data_flow

import (
	"go/token"

	sinks "github.com/kiarash8112/querycontrolleranalyzer/internal/sink_finding"
	tracefunc "github.com/kiarash8112/querycontrolleranalyzer/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

// Phase1_Tabulation seeds sink arguments and walks backward through normal flow.
// Cross-package calls are resolved via ReturnToParamFact instead of a call stack.
func Phase1_Tabulation(
	funcs []*ssa.Function,
	localSinkFacts map[*ssa.Function]*sinks.SinkParamFact,
	localReturnFacts map[*ssa.Function]*tracefunc.ReturnToParamFact,
	loopInfos []sinks.LoopInfo,
	pass *analysis.Pass,
) {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	reported := make(map[token.Pos]bool)

	addPathEdge := func(edge PathEdge) {
		if !P_set[edge] {
			P_set[edge] = true
			worklist = append(worklist, edge)
		}
	}

	for _, fn := range funcs {
		if fn.Synthetic != "" {
			continue
		}
		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				if !isInLoopBounds(call.Pos(), loopInfos) {
					continue
				}
				for _, val := range getCallSinkArgs(call, localSinkFacts, pass) {
					sink := ExplodedNode{Point: tracefunc.ProgramPoint{Block: block, Index: i}, Fact: val}
					addPathEdge(PathEdge{Start: sink, End: sink})
				}
			}
		}
	}

	for len(worklist) > 0 {
		edge := worklist[0]
		worklist = worklist[1:]

		v2 := edge.End.Point
		d2 := edge.End.Fact
		instr := v2.Block.Instrs[v2.Index]

		if d2 == nil {
			continue
		}
		if _, isConst := d2.(*ssa.Const); isConst {
			continue
		}

		if callPoint, argFacts, jumped := tracefunc.ApplyCrossPackageSummary(d2, localReturnFacts, pass); jumped {
			for _, argFact := range argFacts {
				addPathEdge(PathEdge{
					Start: edge.Start,
					End:   ExplodedNode{Point: callPoint, Fact: argFact},
				})
			}
			continue
		}

		for _, nd2 := range tracefunc.ApplyNormalFlow(instr, d2) {
			for _, prevPoint := range tracefunc.GetPredecessors(v2) {
				if loop := innermostLoopFor(tracefunc.ProgramPointPos(edge.Start.Point), loopInfos); loop != nil {
					if isLoopIndexedAccess(instr, d2) {
						reportNPlusOneAtSink(pass, edge.Start, reported)
					}
				}
				addPathEdge(PathEdge{
					Start: edge.Start,
					End:   ExplodedNode{Point: prevPoint, Fact: nd2},
				})
			}
		}

	}
}
