package linters

import (
	"encoding/gob"
	"fmt"
	"go/ast"
	"go/token"
	"reflect"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

// ---------------------------------------------------------
// 1. FACT DEFINITIONS (Cross-Package Summaries)
// ---------------------------------------------------------

type SinkParamFact struct {
	SinkIndices []int
}

func (s *SinkParamFact) AFact()         {}
func (s *SinkParamFact) String() string { return "SinkParamFact" }

type ReturnToParamFact struct {
	ResultToParams map[int][]int
}

func (r *ReturnToParamFact) AFact()         {}
func (r *ReturnToParamFact) String() string { return "ReturnToParamFact" }

// ---------------------------------------------------------
// 2. PLUGIN REGISTRATION
// ---------------------------------------------------------

func init() {
	register.Plugin("nplusone", New)
	gob.Register(&SinkParamFact{})
	gob.Register(&ReturnToParamFact{})
}

type PluginNPlusOne struct{}

func New(settings any) (register.LinterPlugin, error) { return &PluginNPlusOne{}, nil }
func (p *PluginNPlusOne) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{Analyzer}, nil
}
func (p *PluginNPlusOne) GetLoadMode() string { return register.LoadModeTypesInfo }

// ---------------------------------------------------------
// 3. ANALYZER DEFINITION
// ---------------------------------------------------------

var Analyzer = &analysis.Analyzer{
	Name: "nplusone",
	Doc:  "detects true data-dependent N+1 database queries via PathEdge IFDS",
	Run:  run,
	Requires: []*analysis.Analyzer{
		buildssa.Analyzer,
	},
	FactTypes: []analysis.Fact{
		new(SinkParamFact),
		new(ReturnToParamFact),
	},
}

type LoopRange struct {
	Start token.Pos
	End   token.Pos
}

// ---------------------------------------------------------
// 4. MAIN RUN FUNCTION
// ---------------------------------------------------------

func run(pass *analysis.Pass) (any, error) {
	ssaResult := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)

	// Step A: Collect Loop Boundaries
	var loopRanges []LoopRange
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch loop := n.(type) {
			case *ast.ForStmt, *ast.RangeStmt:
				loopRanges = append(loopRanges, LoopRange{Start: loop.Pos(), End: loop.End()})
			}
			return true
		})
	}

	localSinkFacts := make(map[*ssa.Function]*SinkParamFact)
	localReturnFacts := make(map[*ssa.Function]*ReturnToParamFact)
	funcs := ssaResult.SrcFuncs

	// Step B: Build intra-package Facts
	changed := true
	for iterations := 0; changed && iterations < 10; iterations++ {
		changed = false
		for _, fn := range funcs {
			newRetFact := buildReturnFact(fn, localReturnFacts, pass)
			if !reflect.DeepEqual(localReturnFacts[fn], newRetFact) {
				localReturnFacts[fn] = newRetFact
				changed = true
			}

			newSinkFact := buildSinkFact(fn, localSinkFacts, localReturnFacts, pass)
			if !reflect.DeepEqual(localSinkFacts[fn], newSinkFact) {
				localSinkFacts[fn] = newSinkFact
				changed = true
			}
		}
	}

	// Step C: Export Facts
	isTestEnv := pass.Pkg.Name() != "main" && (pass.Pkg.Path() == "todo" || pass.Pkg.Path() == "testdata")
	if !isTestEnv {
		for _, fn := range funcs {
			obj := fn.Object()
			if obj != nil {
				if sf, ok := localSinkFacts[fn]; ok && sf != nil {
					pass.ExportObjectFact(obj, sf)
				}
				if rf, ok := localReturnFacts[fn]; ok && rf != nil {
					pass.ExportObjectFact(obj, rf)
				}
			}
		}
	}

	// Step D: Detect Vulnerabilities
	for _, fn := range funcs {
		if fn.Name() == "processoptions" || fn.Name() == "processOptions" {
			pos := fn.Pos()
			file := pass.Fset.File(pos)
			fmt.Printf("🔍 FOUND processoptions IN FILE: %s\n", file.Name())
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				callInstr, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}

				pos := callInstr.Pos()
				if !pos.IsValid() || !isInLoopBounds(pos, loopRanges) {
					continue
				}

				sinkIndices := getCallSinkIndices(callInstr, localSinkFacts, pass)
				if len(sinkIndices) > 0 {
					tracer := NewIFDSTracer(fn, localReturnFacts, pass)

					for _, targetIdx := range sinkIndices {
						if targetIdx >= len(callInstr.Common().Args) {
							continue
						}
						argVal := callInstr.Common().Args[targetIdx]

						// Start Tabulation
						_, hitLoop := tracer.Tabulate(callInstr.(ssa.Instruction), argVal, loopRanges)

						if hitLoop {
							pass.Report(analysis.Diagnostic{
								Pos:      pos,
								Message:  "🚨 [TRUE N+1] Found dynamic database execution in loop (detected via dataflow)",
								Category: "nplusone",
							})
							break
						}
					}
				}
			}
		}
	}

	return nil, nil
}

// ---------------------------------------------------------
// 5. IFDS PATH-EDGE TABULATION ENGINE
// ---------------------------------------------------------

type ExplodedNode struct {
	Instr ssa.Instruction
	Fact  ssa.Value
}

type PathEdge struct {
	Start ExplodedNode
	End   ExplodedNode
}

type IFDSTracer struct {
	fn       *ssa.Function
	localRet map[*ssa.Function]*ReturnToParamFact
	pass     *analysis.Pass

	P_set    map[PathEdge]bool
	worklist []PathEdge
}

func NewIFDSTracer(fn *ssa.Function, localRet map[*ssa.Function]*ReturnToParamFact, pass *analysis.Pass) *IFDSTracer {
	return &IFDSTracer{
		fn:       fn,
		localRet: localRet,
		pass:     pass,
		P_set:    make(map[PathEdge]bool),
		worklist: make([]PathEdge, 0),
	}
}

func (t *IFDSTracer) addPathEdge(edge PathEdge) {
	if !t.P_set[edge] {
		t.P_set[edge] = true
		t.worklist = append(t.worklist, edge)
	}
}

// Tabulate runs the formal Worklist algorithm using PathEdges
func (t *IFDSTracer) Tabulate(startInstr ssa.Instruction, startFact ssa.Value, loopRanges []LoopRange) ([]int, bool) {
	startNode := ExplodedNode{Instr: startInstr, Fact: startFact}
	t.addPathEdge(PathEdge{Start: startNode, End: startNode})

	hitLoop := false
	paramsReached := make(map[int]bool)

	for len(t.worklist) > 0 {
		edge := t.worklist[0]
		t.worklist = t.worklist[1:]

		v2 := edge.End.Instr
		d2 := edge.End.Fact

		if d2 == nil {
			continue
		}

		// 1. Target Conditions
		if isLoopIterator(d2, loopRanges) {
			hitLoop = true
			continue // Path terminates at vulnerability
		}
		if param, ok := d2.(*ssa.Parameter); ok {
			if idx := indexOfParam(t.fn, param); idx >= 0 {
				paramsReached[idx] = true
			}
			continue // Path terminates at function boundary
		}
		if _, isConst := d2.(*ssa.Const); isConst {
			continue // Path terminates safely
		}

		// 2. Data Flow Resolutions
		if call, ok := d2.(*ssa.Call); ok {

			// --- CROSS-PACKAGE SUMMARY JUMP ---
			// E.g., `id := pk1.getuserID(user)`
			// We check if `pk1.getuserID` has an exported ReturnFact
			retFact := t.getReturnFact(call.Call.StaticCallee())

			if retFact != nil {
				// We drop `id` and instantly create a PathEdge for `user` based on the Summary map
				for _, mappedParams := range retFact.ResultToParams {
					for _, pIdx := range mappedParams {
						if pIdx < len(call.Call.Args) {
							argFact := call.Call.Args[pIdx]
							t.addPathEdge(PathEdge{
								Start: edge.Start,
								End:   ExplodedNode{Instr: call, Fact: argFact},
							})
						}
					}
				}
				continue
			}
		}

		// 3. Standard Instruction Flow
		predecessors := getPredecessorFacts(d2)
		for _, pFact := range predecessors {
			var pInstr ssa.Instruction
			if instr, isInstr := pFact.(ssa.Instruction); isInstr {
				pInstr = instr // The predecessor value is an instruction itself
			} else {
				pInstr = v2 // Fallback to current context
			}

			t.addPathEdge(PathEdge{
				Start: edge.Start,
				End:   ExplodedNode{Instr: pInstr, Fact: pFact},
			})
		}
	}

	var res []int
	for p := range paramsReached {
		res = append(res, p)
	}
	return res, hitLoop
}

func (t *IFDSTracer) getReturnFact(callee *ssa.Function) *ReturnToParamFact {
	if callee == nil {
		return nil
	}
	if fr, ok := t.localRet[callee]; ok {
		return fr
	}
	if obj := callee.Object(); obj != nil {
		var exportedFact ReturnToParamFact
		if t.pass.ImportObjectFact(obj, &exportedFact) {
			return &exportedFact
		}
	}
	return nil
}

// ---------------------------------------------------------
// 6. HELPER DATA FLOW MAPPINGS
// ---------------------------------------------------------

// getPredecessorFacts acts as the transfer function tracking a variable to its definition
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
