package linters

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

type ExplodedNode struct {
	Point ProgramPoint
	Fact  ssa.Value
}

type PathEdge struct {
	Start ExplodedNode
	End   ExplodedNode
}

// Phase1_Tabulation seeds sink arguments and walks backward through normal flow.
// Cross-package calls are resolved via ReturnToParamFact instead of a call stack.
func Phase1_Tabulation(
	funcs []*ssa.Function,
	localSinkFacts map[*ssa.Function]*SinkParamFact,
	localReturnFacts map[*ssa.Function]*ReturnToParamFact,
	pass *analysis.Pass,
) map[token.Pos][]ssa.Value {
	P_set := make(map[PathEdge]bool)
	var worklist []PathEdge
	allResolutions := make(map[token.Pos][]ssa.Value)

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
				for _, sinkArgIdx := range getCallSinkIndices(call, localSinkFacts, pass) {
					if sinkArgIdx >= len(call.Common().Args) {
						continue
					}
					val := call.Common().Args[sinkArgIdx]
					if call.Pos().IsValid() {
						allResolutions[call.Pos()] = append(allResolutions[call.Pos()], val)
					}
					sink := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
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

		if callPoint, argFacts, jumped := applyCrossPackageSummary(d2, localReturnFacts, pass); jumped {
			for _, argFact := range argFacts {
				addPathEdge(PathEdge{
					Start: edge.Start,
					End:   ExplodedNode{Point: callPoint, Fact: argFact},
				})
			}
			continue
		}

		if callInstr, ok := instr.(ssa.CallInstruction); ok {
			for _, nd2 := range applyCallToReturn(callInstr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					addPathEdge(PathEdge{
						Start: edge.Start,
						End:   ExplodedNode{Point: prevPoint, Fact: nd2},
					})
				}
			}
		} else if isEntryNode(v2) {
			fn := v2.Block.Parent()
			for paramIdx, param := range fn.Params {
				if param != d2 {
					continue
				}
				for _, caller := range getPackageCallers(fn, funcs) {
					if paramIdx >= len(caller.Common().Args) {
						continue
					}
					arg := caller.Common().Args[paramIdx]
					if caller.Pos().IsValid() {
						allResolutions[caller.Pos()] = append(allResolutions[caller.Pos()], arg)
					}
					callerPoint := getInstructionPoint(caller)
					for _, prevPoint := range getPredecessors(callerPoint) {
						addPathEdge(PathEdge{
							Start: ExplodedNode{Point: prevPoint, Fact: arg},
							End:   ExplodedNode{Point: prevPoint, Fact: arg},
						})
					}
				}
			}
		} else {
			for _, nd2 := range applyNormalFlow(instr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					addPathEdge(PathEdge{
						Start: edge.Start,
						End:   ExplodedNode{Point: prevPoint, Fact: nd2},
					})
				}
			}
		}
	}

	return allResolutions
}

func verifyNPlusOne(
	pass *analysis.Pass,
	allResolutions map[token.Pos][]ssa.Value,
	loopRanges []LoopRange,
) {
	for pos, resolvedArgs := range allResolutions {
		if !isInLoopBounds(pos, loopRanges) {
			continue
		}

		isDynamic := false
		for _, arg := range resolvedArgs {
			if _, isConst := arg.(*ssa.Const); !isConst {
				isDynamic = true
				break
			}
		}

		if isDynamic {
			pass.Report(analysis.Diagnostic{
				Pos:      pos,
				Message:  "🚨 [TRUE N+1] Found dynamic database execution in loop (detected via dataflow)",
				Category: "nplusone",
			})
		}
	}
}
