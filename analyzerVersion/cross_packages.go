package linters

import (
	"reflect"

	"github.com/kiarash8112/querycontrolleranalyzer/internal/return_to_params"
	sinks "github.com/kiarash8112/querycontrolleranalyzer/internal/sink_finding"
	tracefunc "github.com/kiarash8112/querycontrolleranalyzer/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

func createCrossPackageFacts(pass *analysis.Pass) (map[*ssa.Function]*sinks.SinkParamFact, map[*ssa.Function]*tracefunc.ReturnToParamFact) {
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	localSinkFacts := make(map[*ssa.Function]*sinks.SinkParamFact)
	localReturnFacts := make(map[*ssa.Function]*tracefunc.ReturnToParamFact)

	funcs := ssaResult.SrcFuncs
	changed := true
	for iterations := 0; changed && iterations < 10; iterations++ {
		changed = false

		for _, fn := range funcs {
			newRetFact := return_to_params.BuildReturnFact(fn, localReturnFacts, pass)
			if !reflect.DeepEqual(localReturnFacts[fn], newRetFact) {
				localReturnFacts[fn] = newRetFact
				changed = true
			}
		}

		directSinkFacts := make(map[*ssa.Function]*sinks.SinkParamFact, len(funcs))
		for _, fn := range funcs {
			directSinkFacts[fn] = sinks.BuildDirectSinkFact(fn, localReturnFacts, pass)
		}

		newSinkFacts := sinks.PropagateSinkToPackageCallers(funcs, directSinkFacts, localReturnFacts, pass)
		for _, fn := range funcs {
			if !reflect.DeepEqual(localSinkFacts[fn], newSinkFacts[fn]) {
				localSinkFacts[fn] = newSinkFacts[fn]
				changed = true
			}
		}
	}

	executorSet := sinks.ComputeExecutorSet(funcs, pass)

	for _, fn := range funcs {
		obj := fn.Object()
		if obj != nil {
			if sf, ok := localSinkFacts[fn]; ok && sf != nil {
				pass.ExportObjectFact(obj, sf)
			}
			if rf, ok := localReturnFacts[fn]; ok && rf != nil {
				pass.ExportObjectFact(obj, rf)
			}
			if executorSet[fn] {
				pass.ExportObjectFact(obj, &sinks.ExecutorFact{})
			}
		}
	}

	return localSinkFacts, localReturnFacts
}
