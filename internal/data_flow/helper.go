package data_flow

import (
	"go/token"

	sinks "github.com/kiarash8112/query-controller/internal/sink_finding"
	tracefunc "github.com/kiarash8112/query-controller/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func innermostLoopFor(pos token.Pos, loops []sinks.LoopInfo) *sinks.LoopInfo {
	if !pos.IsValid() {
		return nil
	}
	var best *sinks.LoopInfo
	bestSpan := -1
	for i := range loops {
		l := &loops[i]
		if pos >= l.Start && pos <= l.End {
			span := int(l.End - l.Start)
			if best == nil || span < bestSpan {
				best = l
				bestSpan = span
			}
		}
	}
	return best
}

func isLoopIndexedAccess(instr ssa.Instruction, fact ssa.Value) bool {
	instrVal, ok := instr.(ssa.Value)
	if !ok || instrVal != fact {
		return false
	}

	switch val := instr.(type) {
	case *ssa.Phi:
		return true
	case *ssa.IndexAddr:
		return indexDependsOnPhi(val.Index)
	case *ssa.Index:
		return indexDependsOnPhi(val.Index)
	default:
		return false
	}
}

func indexDependsOnPhi(v ssa.Value) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(*ssa.Phi); ok {
		return true
	}
	if bin, ok := v.(*ssa.BinOp); ok {
		if _, ok := bin.X.(*ssa.Phi); ok {
			return true
		}
		if _, ok := bin.Y.(*ssa.Phi); ok {
			return true
		}
	}
	return false
}

func reportNPlusOneAtSink(pass *analysis.Pass, sink ExplodedNode, reported map[token.Pos]bool) {
	pos := tracefunc.ProgramPointPos(sink.Point)
	if !pos.IsValid() || reported[pos] {
		return
	}
	reported[pos] = true
	pass.Report(analysis.Diagnostic{
		Pos:      pos,
		Message:  "🚨 [TRUE N+1] Found dynamic database execution in loop (detected via dataflow)",
		Category: "nplusone",
	})
}

func isInLoopBounds(pos token.Pos, loops []sinks.LoopInfo) bool {
	if !pos.IsValid() {
		return false
	}
	for _, lr := range loops {
		if pos >= lr.Start && pos <= lr.End {
			return true
		}
	}
	return false
}

func getSinkFact(callee *ssa.Function, localSinks map[*ssa.Function]*sinks.SinkParamFact, pass *analysis.Pass) *sinks.SinkParamFact {
	if callee == nil {
		return nil
	}
	if sf, ok := localSinks[callee]; ok {
		return sf
	}
	if obj := callee.Object(); obj != nil {
		var exportedFact sinks.SinkParamFact
		if pass.ImportObjectFact(obj, &exportedFact) {
			return &exportedFact
		}
	}
	return nil
}

func getCallSinkArgs(call ssa.CallInstruction, localSinks map[*ssa.Function]*sinks.SinkParamFact, pass *analysis.Pass) []ssa.Value {
	if args := sinks.GetGormSinkArgs(call); len(args) > 0 {
		return args
	}

	callee := call.Common().StaticCallee()
	if sf := getSinkFact(callee, localSinks, pass); sf != nil {
		var vals []ssa.Value
		for _, idx := range sf.SinkIndices {
			if idx < len(call.Common().Args) {
				vals = append(vals, call.Common().Args[idx])
			}
		}
		return vals
	}
	return nil
}
