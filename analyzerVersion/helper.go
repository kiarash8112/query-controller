package linters

import (
	"go/token"
	"reflect"
	"slices"

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
	//todo: is possible to see callee in same package that has not be ready yet ?
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

func getExecutorFact(callee *ssa.Function, pass *analysis.Pass) bool {
	if callee == nil {
		return false
	}
	obj := callee.Object()
	if obj == nil {
		return false
	}
	var fact ExecutorFact
	return pass.ImportObjectFact(obj, &fact)
}

func computeExecutorSet(funcs []*ssa.Function, pass *analysis.Pass) map[*ssa.Function]bool {
	execMap := make(map[*ssa.Function]bool)

	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				if isExecutionMethod(getCallNameFromInstr(call)) {
					execMap[fn] = true
					continue
				}
				if callee := call.Common().StaticCallee(); callee != nil && getExecutorFact(callee, pass) {
					execMap[fn] = true
				}
			}
		}
	}

	for changed := true; changed; {
		changed = false
		for _, fn := range funcs {
			if execMap[fn] {
				continue
			}
			for _, b := range fn.Blocks {
				for _, instr := range b.Instrs {
					call, ok := instr.(ssa.CallInstruction)
					if !ok {
						continue
					}
					callee := call.Common().StaticCallee()
					if callee != nil && execMap[callee] {
						execMap[fn] = true
						changed = true
					}
				}
			}
		}
	}

	return execMap
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

func sortedSinkIndices(indices map[int]bool) []int {
	if len(indices) == 0 {
		return nil
	}
	res := make([]int, 0, len(indices))
	for idx := range indices {
		res = append(res, idx)
	}
	slices.Sort(res)
	return res
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
	res := sortedSinkIndices(indices)
	if res == nil {
		return nil
	}
	return &SinkParamFact{SinkIndices: res}
}

func sinkFactFromParams(params map[int]bool) *SinkParamFact {
	res := sortedSinkIndices(params)
	if res == nil {
		return nil
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

			for _, argVal := range getGormSinkArgs(call) {
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
			spreadCompositeValue(fn, d2, v2, &worklist, paramsReached)
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

func getCallNameFromInstr(call ssa.CallInstruction) string {
	if call.Common().Method != nil {
		return call.Common().Method.Name()
	}
	if callee := call.Common().StaticCallee(); callee != nil {
		return callee.Name()
	}
	return ""
}

func buildTransitiveExecutors(funcs []*ssa.Function, pass *analysis.Pass) map[string]bool {
	execMap := computeExecutorSet(funcs, pass)

	names := make(map[string]bool)
	for fn := range execMap {
		names[fn.Name()] = true
	}
	for _, fn := range funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				callee := call.Common().StaticCallee()
				if callee != nil && getExecutorFact(callee, pass) {
					names[callee.Name()] = true
				}
			}
		}
	}
	return names
}

func getGormSinkArgs(call ssa.CallInstruction) []ssa.Value {
	methodName := getCallNameFromInstr(call)
	switch methodName {
	case "Where", "Raw", "Not", "Or", "Select", "Having", "Group", "Order", "Query", "QueryRow", "Exec":
		if len(call.Common().Args) > 1 {
			return call.Common().Args[1:]
		}
	}
	return nil
}

func getCallSinkArgs(call ssa.CallInstruction, localSinks map[*ssa.Function]*SinkParamFact, pass *analysis.Pass) []ssa.Value {
	if args := getGormSinkArgs(call); len(args) > 0 {
		return args
	}

	callee := call.Common().StaticCallee()
	if sf := getSinkFact(callee, localSinks, pass); sf != nil {
		var vals []ssa.Value
		for _, idx := range sf.SinkIndices {
			if idx < len(call.Common().Args) {
				vals = append(vals, call.Common().Args[idx])
			}
		}
		return vals
	}
	return nil
}

func isExecutionMethod(name string) bool {
	switch name {
	case "Scan", "Find", "First", "Take", "Last", "Pluck", "Count", "Exec", "QueryRow", "Query":
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

func isLoopIndexedAccess(instr ssa.Instruction, fact ssa.Value) bool {
	instrVal, ok := instr.(ssa.Value)
	if !ok || instrVal != fact {
		return false
	}

	switch val := instr.(type) {
	case *ssa.Phi:
		return true
	case *ssa.IndexAddr:
		return indexDependsOnPhi(val.Index)
	case *ssa.Index:
		return indexDependsOnPhi(val.Index)
	default:
		return false
	}
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
