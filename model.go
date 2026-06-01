package main

import (
	"go/token"

	"golang.org/x/tools/go/ssa"
)

const MaxCallStackDepth = 7

type LoopRange struct {
	Start token.Pos
	End   token.Pos
}

type ASTLoopVisitor struct {
	inLoop     bool
	isExecLoop bool
	Executors  map[string]bool
	LoopRanges *[]LoopRange
}

type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}
type ExplodedNode struct {
	Point ProgramPoint
	Fact  ssa.Value
}
type StackFrame struct {
	Call  ssa.CallInstruction
	Start ExplodedNode
}
type PathEdge struct {
	Start     ExplodedNode
	End       ExplodedNode
	CallStack []StackFrame
}
