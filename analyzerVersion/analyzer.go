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

type LoopInfo struct {
	Start token.Pos
	End   token.Pos
}

func astContainsExecutionMethod(node ast.Node, executors map[string]bool) bool {
	hasExecution := false
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		case *ast.Ident:
			name = fun.Name
		}
		if isExecutionMethod(name) || executors[name] {
			hasExecution = true
			return false
		}
		return true
	})
	return hasExecution
}

func collectLoopInfo(pass *analysis.Pass, executors map[string]bool) []LoopInfo {
	var loops []LoopInfo
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch loop := n.(type) {
			case *ast.ForStmt:
				if astContainsExecutionMethod(loop.Body, executors) {
					loops = append(loops, LoopInfo{Start: loop.Pos(), End: loop.End()})
				}
			case *ast.RangeStmt:
				if astContainsExecutionMethod(loop.Body, executors) {
					loops = append(loops, LoopInfo{Start: loop.Pos(), End: loop.End()})
				}
			}
			return true
		})
	}

	return loops
}

func run(pass *analysis.Pass) (any, error) {
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	funcs := ssaResult.SrcFuncs
	executors := buildTransitiveExecutors(funcs)
	loopInfos := collectLoopInfo(pass, executors)

	localSinkFacts, localReturnFacts := createCrossPackageFacts(pass)

	Phase1_Tabulation(funcs, localSinkFacts, localReturnFacts, loopInfos, pass)

	return nil, nil
}
