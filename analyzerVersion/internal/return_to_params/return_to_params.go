package return_to_params

import (
	tracefunc "github.com/kiarash8112/querycontrolleranalyzer/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func BuildReturnFact(fn *ssa.Function, localRet map[*ssa.Function]*tracefunc.ReturnToParamFact, pass *analysis.Pass) *tracefunc.ReturnToParamFact {
	resMap := make(map[int][]int)

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if ret, ok := instr.(*ssa.Return); ok {
				for retIdx, retVal := range ret.Results {
					paramsReached := tracefunc.TraceToParams(fn, ret, retVal, localRet, pass)
					if len(paramsReached) > 0 {
						resMap[retIdx] = append(resMap[retIdx], paramsReached...)
					}
				}
			}
		}
	}
	if len(resMap) == 0 {
		return nil
	}
	return &tracefunc.ReturnToParamFact{ResultToParams: resMap}
}
