package linters

import (
	"reflect"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

type SinkParamFact struct {
	SinkIndices []int
}

func (s *SinkParamFact) AFact()         {}
func (s *SinkParamFact) String() string { return "SinkParamFact" }

type ReturnToParamFact struct {
	ResultToParams map[int][]int
}

func (r *ReturnToParamFact) AFact()         {}
func (r *ReturnToParamFact) String() string { return "ReturnToParamFact" }

func createCrossPackageFacts(pass *analysis.Pass) (map[*ssa.Function]*SinkParamFact, map[*ssa.Function]*ReturnToParamFact) {
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	localSinkFacts := make(map[*ssa.Function]*SinkParamFact)
	localReturnFacts := make(map[*ssa.Function]*ReturnToParamFact)

	funcs := ssaResult.SrcFuncs
	// Step B: Build intra-package Facts
	changed := true
	for iterations := 0; changed && iterations < 10; iterations++ {
		changed = false
		for _, fn := range funcs {
			newRetFact := buildReturnFact(fn, localReturnFacts, pass)
			if !reflect.DeepEqual(localReturnFacts[fn], newRetFact) {
				localReturnFacts[fn] = newRetFact
				changed = true
			}

			newSinkFact := buildSinkFact(fn, localSinkFacts, localReturnFacts, pass)
			if !reflect.DeepEqual(localSinkFacts[fn], newSinkFact) {
				localSinkFacts[fn] = newSinkFact
				changed = true
			}
		}
	}

	// Step C: Export Facts
	isTestEnv := pass.Pkg.Name() != "main" && (pass.Pkg.Path() == "todo" || pass.Pkg.Path() == "testdata")
	if !isTestEnv {
		for _, fn := range funcs {
			obj := fn.Object()
			if obj != nil {
				if sf, ok := localSinkFacts[fn]; ok && sf != nil {
					pass.ExportObjectFact(obj, sf)
				}
				if rf, ok := localReturnFacts[fn]; ok && rf != nil {
					pass.ExportObjectFact(obj, rf)
				}
			}
		}
	}

	return localSinkFacts, localReturnFacts
}
