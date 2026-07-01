package tracefunc

import "golang.org/x/tools/go/ssa"

type ReturnToParamFact struct {
	ResultToParams map[int][]int
}

func (r *ReturnToParamFact) AFact()         {}
func (r *ReturnToParamFact) String() string { return "ReturnToParamFact" }

type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}

type traceNode struct {
	Point ProgramPoint
	Fact  ssa.Value
}
