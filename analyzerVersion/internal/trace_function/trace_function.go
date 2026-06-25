package tracefunc

import (
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func applyCallToReturn(call ssa.CallInstruction, fact ssa.Value) []ssa.Value {
	if callVal, ok := call.(ssa.Value); ok && callVal == fact {
		return nil
	}
	return []ssa.Value{fact}
}

func ApplyNormalFlow(instr ssa.Instruction, d2 ssa.Value) []ssa.Value {
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

func ApplyCrossPackageSummary(
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

func TraceToParams(fn *ssa.Function, startInstr ssa.Instruction, startFact ssa.Value, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) []int {
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

		if callPoint, argFacts, jumped := ApplyCrossPackageSummary(d2, localRet, pass); jumped {
			for _, argFact := range argFacts {
				worklist = append(worklist, traceNode{Point: callPoint, Fact: argFact})
			}
			continue
		}

		v2 := node.Point
		instr := v2.Block.Instrs[v2.Index]

		if callInstr, ok := instr.(ssa.CallInstruction); ok {
			for _, nd2 := range applyCallToReturn(callInstr, d2) {
				for _, prevPoint := range GetPredecessors(v2) {
					worklist = append(worklist, traceNode{Point: prevPoint, Fact: nd2})
				}
			}
		} else if isEntryNode(v2) {
			spreadCompositeValue(fn, d2, v2, &worklist, paramsReached)
		} else {
			for _, nd2 := range ApplyNormalFlow(instr, d2) {
				for _, prevPoint := range GetPredecessors(v2) {
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
