package linters

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
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
	Start      token.Pos
	End        token.Pos
	RangeValue ssa.Value
}

func collectLoopInfo(pass *analysis.Pass, funcs []*ssa.Function) []LoopInfo {
	var loops []LoopInfo
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch loop := n.(type) {
			case *ast.ForStmt:
				loops = append(loops, LoopInfo{Start: loop.Pos(), End: loop.End()})
			case *ast.RangeStmt:
				var rangeValue ssa.Value
				if fn := findSSAFunctionForNode(funcs, loop); fn != nil {
					rangeValue = ssaValueForExpr(fn, loop.X, pass.TypesInfo)
				}
				loops = append(loops, LoopInfo{
					Start:      loop.Pos(),
					End:        loop.End(),
					RangeValue: rangeValue,
				})
			}
			return true
		})
	}

	return loops
}

func findSSAFunctionForNode(funcs []*ssa.Function, node ast.Node) *ssa.Function {
	nodePos := node.Pos()
	for _, fn := range funcs {
		if fn.Syntax() == nil {
			continue
		}
		fnPos := fn.Syntax().Pos()
		if nodePos >= fnPos && nodePos <= fn.Syntax().End() {
			return fn
		}
	}
	return nil
}

func ssaValueForExpr(fn *ssa.Function, expr ast.Expr, info *types.Info) ssa.Value {
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	obj := info.ObjectOf(ident)
	if obj == nil {
		return nil
	}
	return ssaValueForObject(fn, obj)
}

func ssaValueForObject(fn *ssa.Function, obj types.Object) ssa.Value {
	for _, param := range fn.Params {
		if param.Object() == obj {
			return param
		}
	}
	for _, freeVar := range fn.FreeVars {
		if freeVar.Name() == obj.Name() && freeVar.Pos() == obj.Pos() {
			return freeVar
		}
	}
	for _, local := range fn.Locals {
		if local.Pos() == obj.Pos() {
			return local
		}
	}
	if fn.Pkg != nil {
		if member, ok := fn.Pkg.Members[obj.Name()]; ok {
			if global, ok := member.(*ssa.Global); ok && global.Object() == obj {
				return global
			}
		}
	}
	return nil
}

func run(pass *analysis.Pass) (any, error) {
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	funcs := ssaResult.SrcFuncs
	loopInfos := collectLoopInfo(pass, funcs)

	localSinkFacts, localReturnFacts := createCrossPackageFacts(pass)

	Phase1_Tabulation(funcs, localSinkFacts, localReturnFacts, loopInfos, pass)

	return nil, nil
}
