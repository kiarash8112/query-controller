package sinks

import "go/token"

type LoopInfo struct {
	Start token.Pos
	End   token.Pos
}

type ExecutorFact struct{}

func (e *ExecutorFact) AFact()         {}
func (e *ExecutorFact) String() string { return "ExecutorFact" }

type SinkParamFact struct {
	SinkIndices []int
}

func (s *SinkParamFact) AFact()         {}
func (s *SinkParamFact) String() string { return "SinkParamFact" }
