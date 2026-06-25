package tracefunc

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func isEntryNode(node ProgramPoint) bool {
	return node.Index == 0 && node.Block.Index == 0
}

func GetPredecessors(p ProgramPoint) []ProgramPoint {
	if p.Index > 0 {
		return []ProgramPoint{{Block: p.Block, Index: p.Index - 1}}
	}
	var preds []ProgramPoint
	for _, predBlock := range p.Block.Preds {
		if len(predBlock.Instrs) > 0 {
			preds = append(preds, ProgramPoint{Block: predBlock, Index: len(predBlock.Instrs) - 1})
		}
	}
	return preds
}

func getInstructionPoint(instr ssa.Instruction) ProgramPoint {
	b := instr.Block()
	for i, inst := range b.Instrs {
		if inst == instr {
			return ProgramPoint{Block: b, Index: i}
		}
	}
	return ProgramPoint{}
}

func getReturnFact(callee *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *ReturnToParamFact {
	if callee == nil {
		return nil
	}
	if fr, ok := localRet[callee]; ok {
		return fr
	}
	//todo: is possible to see callee in same package that has not be ready yet ?
	if obj := callee.Object(); obj != nil {
		var exportedFact ReturnToParamFact
		if pass.ImportObjectFact(obj, &exportedFact) {
			return &exportedFact
		}
	}
	return nil
}

func resolveCallResult(fact ssa.Value) (*ssa.Call, int, bool) {
	switch v := fact.(type) {
	case *ssa.Call:
		return v, 0, true
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok {
			return call, v.Index, true
		}
	}
	return nil, 0, false
}

func tryMarkParam(fn *ssa.Function, v ssa.Value, paramsReached map[int]bool) {
	param, ok := v.(*ssa.Parameter)
	if !ok {
		return
	}
	if idx := indexOfParam(fn, param); idx >= 0 {
		paramsReached[idx] = true
	}
}

func compositeValueOperands(v ssa.Value) ([]ssa.Value, bool) {
	switch val := v.(type) {
	case *ssa.IndexAddr:
		return []ssa.Value{val.X, val.Index}, true
	case *ssa.Index:
		return []ssa.Value{val.X, val.Index}, true
	case *ssa.FieldAddr:
		return []ssa.Value{val.X}, true
	case *ssa.Field:
		return []ssa.Value{val.X}, true
	case *ssa.Lookup:
		return []ssa.Value{val.X, val.Index}, true
	}
	return nil, false
}

func spreadCompositeValue(fn *ssa.Function, d2 ssa.Value, point ProgramPoint, worklist *[]traceNode, paramsReached map[int]bool) bool {
	operands, ok := compositeValueOperands(d2)
	if !ok {
		return false
	}
	if instr, ok := d2.(ssa.Instruction); ok {
		point = getInstructionPoint(instr)
	}
	for _, op := range operands {
		if _, isConst := op.(*ssa.Const); isConst {
			continue
		}
		if _, isParam := op.(*ssa.Parameter); isParam {
			tryMarkParam(fn, op, paramsReached)
			continue
		}
		*worklist = append(*worklist, traceNode{Point: point, Fact: op})
	}
	return true
}

func indexOfParam(fn *ssa.Function, p *ssa.Parameter) int {
	for i, param := range fn.Params {
		if param == p {
			return i
		}
	}
	return -1
}

func ProgramPointPos(p ProgramPoint) token.Pos {
	if p.Index < 0 || p.Index >= len(p.Block.Instrs) {
		return token.NoPos
	}
	return p.Block.Instrs[p.Index].Pos()
}
