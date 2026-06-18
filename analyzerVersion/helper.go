package linters

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

type ProgramPoint struct {
	Block *ssa.BasicBlock
	Index int
}

type traceNode struct {
	Point ProgramPoint
	Fact  ssa.Value
}

func isEntryNode(node ProgramPoint) bool {
	return node.Index == 0 && node.Block.Index == 0
}

func getPredecessors(p ProgramPoint) []ProgramPoint {
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

func applyCallToReturn(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		return nil
	}
	return []ssa.Value{fact}
}

func applyNormalFlow(instr ssa.Instruction, d2 ssa.Value) []ssa.Value {
	if instrVal, ok := instr.(ssa.Value); ok && instrVal == d2 {
		var newFacts []ssa.Value
		for _, opPtr := range instr.Operands(nil) {
			if opPtr != nil && *opPtr != nil {
				newFacts = append(newFacts, *opPtr)
			}
		}
		return newFacts
	}

	if store, ok := instr.(*ssa.Store); ok {
		if store.Addr == d2 {
			return []ssa.Value{d2, store.Val}
		}
		if idx, isIdx := store.Addr.(*ssa.IndexAddr); isIdx && idx.X == d2 {
			return []ssa.Value{d2, store.Val}
		}
		if fld, isFld := store.Addr.(*ssa.FieldAddr); isFld && fld.X == d2 {
			return []ssa.Value{d2, store.Val}
		}
	}
	return []ssa.Value{d2}
}

func getReturnFact(callee *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *ReturnToParamFact {
	if callee == nil {
		return nil
	}
	if fr, ok := localRet[callee]; ok {
		return fr
	}
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

func applyCrossPackageSummary(
	d2 ssa.Value,
	localRet map[*ssa.Function]*ReturnToParamFact,
	pass *analysis.Pass,
) (callPoint ProgramPoint, argFacts []ssa.Value, ok bool) {
	call, retIdx, isCallResult := resolveCallResult(d2)
	if !isCallResult {
		return ProgramPoint{}, nil, false
	}

	retFact := getReturnFact(call.Call.StaticCallee(), localRet, pass)
	if retFact == nil {
		return ProgramPoint{}, nil, false
	}

	mappedParams := retFact.ResultToParams[retIdx]
	if len(mappedParams) == 0 {
		return ProgramPoint{}, nil, false
	}

	callPoint = getInstructionPoint(call)
	for _, pIdx := range mappedParams {
		if pIdx < len(call.Call.Args) {
			argFacts = append(argFacts, call.Call.Args[pIdx])
		}
	}
	return callPoint, argFacts, len(argFacts) > 0
}

func traceToParams(fn *ssa.Function, startInstr ssa.Instruction, startFact ssa.Value, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) []int {
	startPoint := getInstructionPoint(startInstr)
	visited := make(map[traceNode]bool)
	worklist := []traceNode{{Point: startPoint, Fact: startFact}}
	paramsReached := make(map[int]bool)

	for len(worklist) > 0 {
		node := worklist[0]
		worklist = worklist[1:]

		if node.Fact == nil || visited[node] {
			continue
		}
		visited[node] = true

		d2 := node.Fact
		if param, ok := d2.(*ssa.Parameter); ok {
			if idx := indexOfParam(fn, param); idx >= 0 {
				paramsReached[idx] = true
			}
			continue
		}
		if _, isConst := d2.(*ssa.Const); isConst {
			continue
		}

		if callPoint, argFacts, jumped := applyCrossPackageSummary(d2, localRet, pass); jumped {
			for _, argFact := range argFacts {
				worklist = append(worklist, traceNode{Point: callPoint, Fact: argFact})
			}
			continue
		}

		v2 := node.Point
		instr := v2.Block.Instrs[v2.Index]

		if callInstr, ok := instr.(ssa.CallInstruction); ok {
			for _, nd2 := range applyCallToReturn(callInstr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					worklist = append(worklist, traceNode{Point: prevPoint, Fact: nd2})
				}
			}
		} else if isEntryNode(v2) {
			for paramIdx, param := range fn.Params {
				if param == d2 {
					paramsReached[paramIdx] = true
				}
			}
		} else {
			for _, nd2 := range applyNormalFlow(instr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					worklist = append(worklist, traceNode{Point: prevPoint, Fact: nd2})
				}
			}
		}
	}

	var res []int
	for p := range paramsReached {
		res = append(res, p)
	}
	return res
}

func buildReturnFact(fn *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *ReturnToParamFact {
	resMap := make(map[int][]int)

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if ret, ok := instr.(*ssa.Return); ok {
				for retIdx, retVal := range ret.Results {
					paramsReached := traceToParams(fn, ret, retVal, localRet, pass)
					if len(paramsReached) > 0 {
						resMap[retIdx] = append(resMap[retIdx], paramsReached...)
					}
				}
			}
		}
	}
	if len(resMap) == 0 {
		return nil
	}
	return &ReturnToParamFact{ResultToParams: resMap}
}

func buildSinkFact(fn *ssa.Function, localSinks map[*ssa.Function]*SinkParamFact, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *SinkParamFact {
	sinkParams := make(map[int]bool)

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}

			sinkIndices := getCallSinkIndices(call, localSinks, pass)
			for _, sinkArgIdx := range sinkIndices {
				if sinkArgIdx >= len(call.Common().Args) {
					continue
				}
				argVal := call.Common().Args[sinkArgIdx]

				for _, pIdx := range traceToParams(fn, call.(ssa.Instruction), argVal, localRet, pass) {
					sinkParams[pIdx] = true
				}
			}
		}
	}
	if len(sinkParams) == 0 {
		return nil
	}

	var res []int
	for pIdx := range sinkParams {
		res = append(res, pIdx)
	}
	return &SinkParamFact{SinkIndices: res}
}

func getCallSinkIndices(call ssa.CallInstruction, localSinks map[*ssa.Function]*SinkParamFact, pass *analysis.Pass) []int {
	name := ""
	if call.Common().Method != nil {
		name = call.Common().Method.Name()
	} else if callee := call.Common().StaticCallee(); callee != nil {
		name = callee.Name()
	}

	if isExecutionMethod(name) {
		var args []int
		for i := range call.Common().Args {
			args = append(args, i)
		}
		if len(args) > 1 {
			return args[1:]
		}
		return args
	}

	callee := call.Common().StaticCallee()
	if callee != nil {
		if sf, ok := localSinks[callee]; ok && sf != nil {
			return sf.SinkIndices
		}
		if obj := callee.Object(); obj != nil {
			var expSink SinkParamFact
			if pass.ImportObjectFact(obj, &expSink) {
				return expSink.SinkIndices
			}
		}
	}
	return nil
}

func isExecutionMethod(name string) bool {
	switch name {
	case "Scan", "Find", "First", "Take", "Last", "Pluck", "Count", "Exec", "QueryRow", "Query", "Where", "Raw", "Not", "Or":
		return true
	}
	return false
}

func indexOfParam(fn *ssa.Function, p *ssa.Parameter) int {
	for i, param := range fn.Params {
		if param == p {
			return i
		}
	}
	return -1
}

func isInLoopBounds(pos token.Pos, loopRanges []LoopRange) bool {
	if !pos.IsValid() {
		return false
	}
	for _, lr := range loopRanges {
		if pos >= lr.Start && pos <= lr.End {
			return true
		}
	}
	return false
}
