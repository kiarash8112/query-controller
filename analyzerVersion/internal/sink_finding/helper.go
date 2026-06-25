package sinks

import (
	"slices"

	tracefunc "github.com/kiarash8112/querycontrolleranalyzer/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func getExecutorFact(callee *ssa.Function, pass *analysis.Pass) bool {
	if callee == nil {
		return false
	}
	obj := callee.Object()
	if obj == nil {
		return false
	}
	var fact ExecutorFact
	return pass.ImportObjectFact(obj, &fact)
}

func getCallNameFromInstr(call ssa.CallInstruction) string {
	if call.Common().Method != nil {
		return call.Common().Method.Name()
	}
	if callee := call.Common().StaticCallee(); callee != nil {
		return callee.Name()
	}
	return ""
}

func sortedSinkIndices(indices map[int]bool) []int {
	if len(indices) == 0 {
		return nil
	}
	res := make([]int, 0, len(indices))
	for idx := range indices {
		res = append(res, idx)
	}
	slices.Sort(res)
	return res
}

func mergeSinkFacts(a, b *SinkParamFact) *SinkParamFact {
	if a == nil && b == nil {
		return nil
	}
	indices := make(map[int]bool)
	for _, sf := range []*SinkParamFact{a, b} {
		if sf == nil {
			continue
		}
		for _, idx := range sf.SinkIndices {
			indices[idx] = true
		}
	}
	res := sortedSinkIndices(indices)
	if res == nil {
		return nil
	}
	return &SinkParamFact{SinkIndices: res}
}

func getPackageCallers(fn *ssa.Function, funcs []*ssa.Function) []ssa.CallInstruction {
	var callers []ssa.CallInstruction
	for _, cFn := range funcs {
		for _, b := range cFn.Blocks {
			for _, inst := range b.Instrs {
				if callInst, ok := inst.(ssa.CallInstruction); ok && callInst.Common().StaticCallee() == fn {
					callers = append(callers, callInst)
				}
			}
		}
	}
	return callers
}

func sinkFactAtCallSite(
	callerFn *ssa.Function,
	call ssa.CallInstruction,
	calleeSinkParamIndices []int,
	localRet map[*ssa.Function]*tracefunc.ReturnToParamFact,
	pass *analysis.Pass,
) *SinkParamFact {
	sinkParams := make(map[int]bool)
	for _, paramIdx := range calleeSinkParamIndices {
		if paramIdx >= len(call.Common().Args) {
			continue
		}
		argVal := call.Common().Args[paramIdx]
		for _, pIdx := range tracefunc.TraceToParams(callerFn, call.(ssa.Instruction), argVal, localRet, pass) {
			sinkParams[pIdx] = true
		}
	}
	return sinkFactFromParams(sinkParams)
}
