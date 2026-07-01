package linters

import (
	"github.com/kiarash8112/query-controller/internal/data_flow"
	sinks "github.com/kiarash8112/query-controller/internal/sink_finding"
	tracefunc "github.com/kiarash8112/query-controller/internal/trace_function"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
)

var Analyzer = &analysis.Analyzer{
	Name: "nplusone",
	Doc:  "detects true data-dependent N+1 database queries via PathEdge IFDS",
	Run:  run,
	Requires: []*analysis.Analyzer{
		buildssa.Analyzer,
	},
	FactTypes: []analysis.Fact{
		new(sinks.SinkParamFact),
		new(tracefunc.ReturnToParamFact),
		new(sinks.ExecutorFact),
	},
}

func run(pass *analysis.Pass) (any, error) {
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	funcs := ssaResult.SrcFuncs

	executors := sinks.BuildTransitiveExecutors(funcs, pass)
	loopInfos := sinks.CollectLoopInfo(pass, executors)

	localSinkFacts, localReturnFacts := createCrossPackageFacts(pass)

	data_flow.Phase1_Tabulation(funcs, localSinkFacts, localReturnFacts, loopInfos, pass)

	return nil, nil
}
