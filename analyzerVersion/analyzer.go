package linters

import (
	"go/ast"
	"go/token"

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

	for _, fn := range funcs {

		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}

				pos := callInstr.Pos()
				if !pos.IsValid() || !isInLoopBounds(pos, loopRanges) {
					continue
				}

				sinkIndices := getCallSinkIndices(callInstr, localSinkFacts, pass)
				if len(sinkIndices) > 0 {
					tracer := NewIFDSTracer(fn, localReturnFacts, pass)

					for _, targetIdx := range sinkIndices {
						if targetIdx >= len(callInstr.Common().Args) {
							continue
						}
						argVal := callInstr.Common().Args[targetIdx]

						// Start Tabulation
						_, hitLoop := tracer.Tabulate(callInstr.(ssa.Instruction), argVal, loopRanges)

						if hitLoop {
							pass.Report(analysis.Diagnostic{
								Pos:      pos,
								Message:  "🚨 [TRUE N+1] Found dynamic database execution in loop (detected via dataflow)",
								Category: "nplusone",
							})
							break
						}
					}
				}
			}
		}
	}

	return nil, nil
}
