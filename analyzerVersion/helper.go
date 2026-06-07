package linters

import (
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func getPredecessorFacts(fact ssa.Value) []ssa.Value {
	switch v := fact.(type) {
	case *ssa.MakeInterface:
		return []ssa.Value{v.X}
	case *ssa.ChangeType:
		return []ssa.Value{v.X}
	case *ssa.Convert:
		return []ssa.Value{v.X}
	case *ssa.Slice:
		return []ssa.Value{v.X}
	case *ssa.Field:
		return []ssa.Value{v.X}
	case *ssa.FieldAddr:
		return []ssa.Value{v.X}
	case *ssa.UnOp:
		return []ssa.Value{v.X}
	case *ssa.Extract:
		return []ssa.Value{v.Tuple}
	case *ssa.Phi:
		return v.Edges
	case *ssa.Alloc:
		var res []ssa.Value
		if v.Referrers() != nil {
			for _, ref := range *v.Referrers() {
				if store, ok := ref.(*ssa.Store); ok && store.Addr == v {
					res = append(res, store.Val)
				}
				if idxAddr, ok := ref.(*ssa.IndexAddr); ok {
					if idxAddr.Referrers() != nil {
						for _, idxRef := range *idxAddr.Referrers() {
							if store, ok := idxRef.(*ssa.Store); ok && store.Addr == idxAddr {
								res = append(res, store.Val)
							}
						}
					}
				}
			}
		}
		return res
	}
	return nil
}

func buildReturnFact(fn *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *ReturnToParamFact {
	resMap := make(map[int][]int)
	tracer := NewIFDSTracer(fn, localRet, pass)

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if ret, ok := instr.(*ssa.Return); ok {
				for retIdx, retVal := range ret.Results {
					paramsReached, _ := tracer.Tabulate(ret, retVal, nil)
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
	tracer := NewIFDSTracer(fn, localRet, pass)

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

				paramsReached, _ := tracer.Tabulate(call.(ssa.Instruction), argVal, nil)
				for _, pIdx := range paramsReached {
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

func isLoopIterator(val ssa.Value, loopRanges []LoopRange) bool {
	switch t := val.(type) {
	case *ssa.Extract:
		if _, isNext := t.Tuple.(*ssa.Next); isNext {
			return isInLoopBounds(t.Pos(), loopRanges) || isInLoopBounds(t.Parent().Pos(), loopRanges)
		}
	case *ssa.Next:
		return true
	case *ssa.Phi:
		return isInLoopBounds(t.Pos(), loopRanges)

	case *ssa.IndexAddr:
		// Resolve t.Index if it's a BinOp (like i + 1) or directly a Phi node
		return isLoopIndexValue(t.Index, loopRanges)
	}
	return false
}

// Helper to trace the index operand back to the loop-bound Phi node
func isLoopIndexValue(val ssa.Value, loopRanges []LoopRange) bool {
	if val == nil {
		return false
	}

	switch v := val.(type) {
	case *ssa.Phi:
		block := v.Block()
		if block == nil {
			return false
		}

		// 1. Try the current block first (for standard `for i := 0; ...` loops)
		for _, instr := range block.Instrs {
			if instr.Pos().IsValid() {
				if isInLoopBounds(instr.Pos(), loopRanges) {
					return true
				}
				break // We found the location of this block, no need to keep checking it
			}
		}

		// 2. SYNTHETIC BLOCK FALLBACK: Check Successor Blocks (for `range` loops)
		// If the header is synthetic (token.NoPos), we peek at where it branches.
		// One branch goes to the loop body, which WILL have a valid position.
		for _, succ := range block.Succs {
			for _, instr := range succ.Instrs {
				if instr.Pos().IsValid() {
					if isInLoopBounds(instr.Pos(), loopRanges) {
						return true
					}
					break // Move on to the next successor block
				}
			}
		}

		return false

	case *ssa.BinOp:
		// Follow both sides (e.g., i + 1)
		return isLoopIndexValue(v.X, loopRanges) || isLoopIndexValue(v.Y, loopRanges)

	case *ssa.Convert:
		// Strip type conversions
		return isLoopIndexValue(v.X, loopRanges)
	}

	return false
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
