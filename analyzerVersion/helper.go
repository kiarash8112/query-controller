package linters

import (
	"go/token"
	"reflect"

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

func getSinkFact(callee *ssa.Function, localSinks map[*ssa.Function]*SinkParamFact, pass *analysis.Pass) *SinkParamFact {
	if callee == nil {
		return nil
	}
	if sf, ok := localSinks[callee]; ok {
		return sf
	}
	if obj := callee.Object(); obj != nil {
		var exportedFact SinkParamFact
		if pass.ImportObjectFact(obj, &exportedFact) {
			return &exportedFact
		}
	}
	return nil
}

func getPackageCallers(fn *ssa.Function, funcs []*ssa.Function) []ssa.CallInstruction {
	var callers []ssa.CallInstruction
	for _, cFn := range funcs {
		for _, b := range cFn.Blocks {
			for _, inst := range b.Instrs {
				if callInst, ok := inst.(ssa.CallInstruction); ok && callInst.Common().StaticCallee() == fn {
					callers = append(callers, callInst)
				}
			}
		}
	}
	return callers
}

func mergeSinkFacts(a, b *SinkParamFact) *SinkParamFact {
	if a == nil && b == nil {
		return nil
	}
	indices := make(map[int]bool)
	for _, sf := range []*SinkParamFact{a, b} {
		if sf == nil {
			continue
		}
		for _, idx := range sf.SinkIndices {
			indices[idx] = true
		}
	}
	if len(indices) == 0 {
		return nil
	}
	var res []int
	for idx := range indices {
		res = append(res, idx)
	}
	return &SinkParamFact{SinkIndices: res}
}

func sinkFactFromParams(params map[int]bool) *SinkParamFact {
	if len(params) == 0 {
		return nil
	}
	var res []int
	for pIdx := range params {
		res = append(res, pIdx)
	}
	return &SinkParamFact{SinkIndices: res}
}

func buildDirectSinkFact(fn *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *SinkParamFact {
	sinkParams := make(map[int]bool)

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}

			for _, sinkArgIdx := range getDirectSinkArgIndices(call) {
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
	return sinkFactFromParams(sinkParams)
}

func sinkFactAtCallSite(
	callerFn *ssa.Function,
	call ssa.CallInstruction,
	calleeSinkParamIndices []int,
	localRet map[*ssa.Function]*ReturnToParamFact,
	pass *analysis.Pass,
) *SinkParamFact {
	sinkParams := make(map[int]bool)
	for _, paramIdx := range calleeSinkParamIndices {
		if paramIdx >= len(call.Common().Args) {
			continue
		}
		argVal := call.Common().Args[paramIdx]
		for _, pIdx := range traceToParams(callerFn, call.(ssa.Instruction), argVal, localRet, pass) {
			sinkParams[pIdx] = true
		}
	}
	return sinkFactFromParams(sinkParams)
}

func propagateSinkToPackageCallers(
	funcs []*ssa.Function,
	sinkFacts map[*ssa.Function]*SinkParamFact,
	localRet map[*ssa.Function]*ReturnToParamFact,
	pass *analysis.Pass,
) map[*ssa.Function]*SinkParamFact {
	result := make(map[*ssa.Function]*SinkParamFact, len(funcs))
	for fn, sf := range sinkFacts {
		result[fn] = sf
	}

	for {
		changed := false
		for _, sinkFn := range funcs {
			sf := result[sinkFn]
			if sf == nil || len(sf.SinkIndices) == 0 {
				continue
			}
			for _, caller := range getPackageCallers(sinkFn, funcs) {
				callerFn := caller.Block().Parent()
				atCall := sinkFactAtCallSite(callerFn, caller, sf.SinkIndices, localRet, pass)
				merged := mergeSinkFacts(result[callerFn], atCall)
				if !reflect.DeepEqual(result[callerFn], merged) {
					result[callerFn] = merged
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return result
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

func getDirectSinkArgIndices(call ssa.CallInstruction) []int {
	name := ""
	if call.Common().Method != nil {
		name = call.Common().Method.Name()
	} else if callee := call.Common().StaticCallee(); callee != nil {
		name = callee.Name()
	}

	if !isExecutionMethod(name) {
		return nil
	}

	var args []int
	for i := range call.Common().Args {
		args = append(args, i)
	}
	if len(args) > 1 {
		return args[1:]
	}
	return args
}

func getCallSinkIndices(call ssa.CallInstruction, localSinks map[*ssa.Function]*SinkParamFact, pass *analysis.Pass) []int {
	if indices := getDirectSinkArgIndices(call); len(indices) > 0 {
		return indices
	}

	callee := call.Common().StaticCallee()
	if sf := getSinkFact(callee, localSinks, pass); sf != nil {
		return sf.SinkIndices
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

func isInLoopBounds(pos token.Pos, loops []LoopInfo) bool {
	if !pos.IsValid() {
		return false
	}
	for _, lr := range loops {
		if pos >= lr.Start && pos <= lr.End {
			return true
		}
	}
	return false
}

func programPointPos(p ProgramPoint) token.Pos {
	if p.Index < 0 || p.Index >= len(p.Block.Instrs) {
		return token.NoPos
	}
	return p.Block.Instrs[p.Index].Pos()
}

func innermostLoopFor(pos token.Pos, loops []LoopInfo) *LoopInfo {
	if !pos.IsValid() {
		return nil
	}
	var best *LoopInfo
	bestSpan := -1
	for i := range loops {
		l := &loops[i]
		if pos >= l.Start && pos <= l.End {
			span := int(l.End - l.Start)
			if best == nil || span < bestSpan {
				best = l
				bestSpan = span
			}
		}
	}
	return best
}

func isLoopIndexedAccess(instr ssa.Instruction, fact, nextFact ssa.Value) bool {
	instrVal, ok := instr.(ssa.Value)
	if !ok || instrVal != fact {
		return false
	}

	var index ssa.Value
	switch val := instr.(type) {
	case *ssa.IndexAddr:
		index = val.Index
	case *ssa.Index:
		index = val.Index
	default:
		return false
	}

	return indexDependsOnPhi(index) || indexDependsOnPhi(nextFact)
}

func indexDependsOnPhi(v ssa.Value) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(*ssa.Phi); ok {
		return true
	}
	if bin, ok := v.(*ssa.BinOp); ok {
		if _, ok := bin.X.(*ssa.Phi); ok {
			return true
		}
		if _, ok := bin.Y.(*ssa.Phi); ok {
			return true
		}
	}
	return false
}

func reportNPlusOneAtSink(pass *analysis.Pass, sink ExplodedNode, reported map[token.Pos]bool) {
	pos := programPointPos(sink.Point)
	if !pos.IsValid() || reported[pos] {
		return
	}
	reported[pos] = true
	pass.Report(analysis.Diagnostic{
		Pos:      pos,
		Message:  "🚨 [TRUE N+1] Found dynamic database execution in loop (detected via dataflow)",
		Category: "nplusone",
	})
}
