package linters

import (
	"go/ast"
	"go/token"

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
		new(SinkParamFact),
		new(ReturnToParamFact),
	},
}

type LoopRange struct {
	Start token.Pos
	End   token.Pos
}

func collectLoopBoundries(pass *analysis.Pass) []LoopRange {
	var loopRanges []LoopRange
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch loop := n.(type) {
			case *ast.ForStmt, *ast.RangeStmt:
				loopRanges = append(loopRanges, LoopRange{Start: loop.Pos(), End: loop.End()})
			}
			return true
		})
	}

	return loopRanges
}

func run(pass *analysis.Pass) (any, error) {
	loopRanges := collectLoopBoundries(pass)
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	funcs := ssaResult.SrcFuncs

	localSinkFacts, localReturnFacts := createCrossPackageFacts(pass)

	allResolutions := Phase1_Tabulation(funcs, localSinkFacts, localReturnFacts, pass)
	verifyNPlusOne(pass, allResolutions, loopRanges)

	return nil, nil
}
