package data_flow

import (
	tracefunc "github.com/kiarash8112/querycontrolleranalyzer/internal/trace_function"
	"golang.org/x/tools/go/ssa"
)

type ExplodedNode struct {
	Point tracefunc.ProgramPoint
	Fact  ssa.Value
}

type PathEdge struct {
	Start ExplodedNode
	End   ExplodedNode
}
