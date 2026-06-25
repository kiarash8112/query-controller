package sinks

import (
	"reflect"

	tracefunc "github.com/kiarash8112/querycontrolleranalyzer/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func sinkFactFromParams(params map[int]bool) *SinkParamFact {
	res := sortedSinkIndices(params)
	if res == nil {
		return nil
	}
	return &SinkParamFact{SinkIndices: res}
}

func BuildDirectSinkFact(fn *ssa.Function, localRet map[*ssa.Function]*tracefunc.ReturnToParamFact, pass *analysis.Pass) *SinkParamFact {
	sinkParams := make(map[int]bool)

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}

			for _, argVal := range GetGormSinkArgs(call) {
				for _, pIdx := range tracefunc.TraceToParams(fn, call.(ssa.Instruction), argVal, localRet, pass) {
					sinkParams[pIdx] = true
				}
			}
		}
	}
	return sinkFactFromParams(sinkParams)
}

func PropagateSinkToPackageCallers(
	funcs []*ssa.Function,
	sinkFacts map[*ssa.Function]*SinkParamFact,
	localRet map[*ssa.Function]*tracefunc.ReturnToParamFact,
	pass *analysis.Pass,
) map[*ssa.Function]*SinkParamFact {
	result := make(map[*ssa.Function]*SinkParamFact, len(funcs))
	for fn, sf := range sinkFacts {
		result[fn] = sf
	}

	for {
		changed := false
		for _, sinkFn := range funcs {
			sf := result[sinkFn]
			if sf == nil || len(sf.SinkIndices) == 0 {
				continue
			}
			for _, caller := range getPackageCallers(sinkFn, funcs) {
				callerFn := caller.Block().Parent()
				atCall := sinkFactAtCallSite(callerFn, caller, sf.SinkIndices, localRet, pass)
				merged := mergeSinkFacts(result[callerFn], atCall)
				if !reflect.DeepEqual(result[callerFn], merged) {
					result[callerFn] = merged
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return result
}
