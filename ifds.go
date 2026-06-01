package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"log"
	"os"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	targetDir := "examples/"
	if len(os.Args) > 1 {
		targetDir = os.Args[1]
	}
	fmt.Printf(">> LOADING PROJECT: %s\n", targetDir)

	fset := token.NewFileSet()
	cfg := &packages.Config{
		Fset: fset,
		Dir:  targetDir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
	}

	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Load failed: %v", err)
	}

	prog, _ := ssautil.AllPackages(initial, ssa.NaiveForm|ssa.GlobalDebug)
	prog.Build()

	allFuncs := getAllFunctions(initial, prog)
	executors := buildTransitiveExecutors(allFuncs)

	loopRanges := make([]LoopRange, 0)
	visitor := &ASTLoopVisitor{
		Executors:  executors,
		LoopRanges: &loopRanges,
	}
	for _, pkg := range initial {
		for _, file := range pkg.Syntax {
			ast.Walk(visitor, file)
		}
	}

	// Phase 1: Context-Aware Dataflow Trace (Pure Stack)
	AllResolutions := Phase1_IFDS_Tabulation(allFuncs)

	// Phase 2: Vulnerability Checker
	Phase2_VerifyNPlusOne(AllResolutions, loopRanges, prog.Fset)
}

func Phase1_IFDS_Tabulation(allFunctions []*ssa.Function) map[token.Pos][]ssa.Value {
	P_set := make(map[string]bool)
	var worklist []PathEdge

	AllResolutions := make(map[token.Pos][]ssa.Value)

	addPathEdge := func(edge PathEdge) {
		key := hashEdge(edge)
		if !P_set[key] {
			P_set[key] = true
			worklist = append(worklist, edge)
		}
	}

	// SINK FINDER (Initializes with Empty Stack)
	for _, fn := range allFunctions {
		if fn.Synthetic != "" {
			continue
		}
		for _, block := range fn.Blocks {
			for i, instr := range block.Instrs {
				if call, ok := instr.(ssa.CallInstruction); ok {
					targetArgs := getGormSinkArgs(call)
					for _, val := range targetArgs {
						if call.Pos().IsValid() {
							AllResolutions[call.Pos()] = append(AllResolutions[call.Pos()], val)
						}
						sink := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: val}
						addPathEdge(PathEdge{Start: sink, End: sink, CallStack: nil}) // Empty stack!
					}
				}
			}
		}
	}

	// TABULATION ENGINE
	for len(worklist) > 0 {
		edge := worklist[0]
		worklist = worklist[1:]

		v2 := edge.End.Point
		d2 := edge.End.Fact
		instr := v2.Block.Instrs[v2.Index]

		if callInstr, ok := instr.(ssa.CallInstruction); ok {

			// SCENARIO 1: Tracked variable comes from a Function Call (e.g. id := SafeFunc())
			if callVal, ok := callInstr.(ssa.Value); ok && callVal == d2 {
				callee := callInstr.Common().StaticCallee()
				if callee != nil {

					if len(edge.CallStack) >= MaxCallStackDepth {
						continue
					}

					for _, block := range callee.Blocks {
						for i, retInstr := range block.Instrs {
							if ret, ok := retInstr.(*ssa.Return); ok && len(ret.Results) > 0 {
								calleeD1 := ret.Results[0]

								newCtx := ExplodedNode{Point: ProgramPoint{Block: block, Index: i}, Fact: calleeD1}

								newStack := cloneStack(edge.CallStack)
								newStack = append(newStack, StackFrame{
									Call:  callInstr,
									Start: edge.Start,
								})

								addPathEdge(PathEdge{Start: newCtx, End: newCtx, CallStack: newStack})
							}
						}
					}
				}
			} else {
				// Bypass instruction entirely if it's not what we are tracking
				for _, nd2 := range applyCallToReturn(callInstr, d2) {
					for _, prevPoint := range getPredecessors(v2) {
						addPathEdge(PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}, CallStack: cloneStack(edge.CallStack)})
					}
				}
			}

			// SCENARIO 2: Reached the Top of a Function (Entry Parameters)
		} else if isEntryNode(v2) {
			fn := v2.Block.Parent()
			for paramIdx, param := range fn.Params {
				if param == d2 {

					// 2A) POP THE STACK: We entered to fetch data. Context restores to specific caller.
					if len(edge.CallStack) > 0 {
						lastFrame := edge.CallStack[len(edge.CallStack)-1]
						newStack := cloneStack(edge.CallStack[:len(edge.CallStack)-1])

						callerInstr := lastFrame.Call
						if len(callerInstr.Common().Args) > paramIdx {
							actualArg := callerInstr.Common().Args[paramIdx]

							if callerInstr.Pos().IsValid() {
								AllResolutions[callerInstr.Pos()] = append(AllResolutions[callerInstr.Pos()], actualArg)
							}
							callerPoint := getInstructionPoint(callerInstr)
							for _, prevPoint := range getPredecessors(callerPoint) {
								addPathEdge(PathEdge{
									Start:     lastFrame.Start, // Restoring original Start Node safely!
									End:       ExplodedNode{Point: prevPoint, Fact: actualArg},
									CallStack: newStack,
								})
							}
						}
					} else {
						// 2B) STACK IS EMPTY: This is a Sink-Wrapper that needs to propagate backwards (e.g. GetUser)
						for _, caller := range getGlobalMockCallers(fn, allFunctions) {
							if len(caller.Common().Args) > paramIdx {
								arg := caller.Common().Args[paramIdx]

								if caller.Pos().IsValid() {
									AllResolutions[caller.Pos()] = append(AllResolutions[caller.Pos()], arg)
								}
								callerPoint := getInstructionPoint(caller)
								for _, prevPoint := range getPredecessors(callerPoint) {
									addPathEdge(PathEdge{
										Start:     ExplodedNode{Point: prevPoint, Fact: arg},
										End:       ExplodedNode{Point: prevPoint, Fact: arg},
										CallStack: nil,
									})
								}
							}
						}
					}
				}
			}
		} else {
			// Normal Instruction Flow (Assignments, Constants, math)
			for _, nd2 := range applyNormalFlow(instr, d2) {
				for _, prevPoint := range getPredecessors(v2) {
					addPathEdge(PathEdge{Start: edge.Start, End: ExplodedNode{Point: prevPoint, Fact: nd2}, CallStack: cloneStack(edge.CallStack)})
				}
			}
		}
	}
	return AllResolutions
}

func Phase2_VerifyNPlusOne(AllResolutions map[token.Pos][]ssa.Value, loopRanges []LoopRange, fset *token.FileSet) {
	fmt.Println("\n==========================================")
	fmt.Println(">> PHASE 2: GORM N+1 VULNERABILITY REPORT")
	fmt.Println("==========================================")

	vulnsFound := 0

	isInLoopBounds := func(pos token.Pos) bool {
		for _, lr := range loopRanges {
			if pos >= lr.Start && pos <= lr.End {
				return true
			}
		}
		return false
	}

	for pos, resolvedArgs := range AllResolutions {
		if !isInLoopBounds(pos) {
			continue
		}

		isDynamicVariable := false
		for _, arg := range resolvedArgs {
			if _, isConst := arg.(*ssa.Const); !isConst {
				isDynamicVariable = true
			}
		}

		if isDynamicVariable {
			vulnsFound++
			posString := fset.Position(pos).String()
			fmt.Printf(" 🚨 [TRUE N+1] Found dynamic database execution in loop at \t%s \n", posString)
		}
	}

	if vulnsFound == 0 {
		fmt.Println(" ✅ Project Clean! No N+1 Queries detected.")
	}
}
