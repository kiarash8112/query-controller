package sinks

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ssa"
)

func astContainsExecutionMethod(node ast.Node, executors map[string]bool) bool {
	hasExecution := false
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		case *ast.Ident:
			name = fun.Name
		}
		if isExecutionMethod(name) || executors[name] {
			hasExecution = true
			return false
		}
		return true
	})
	return hasExecution
}

func CollectLoopInfo(pass *analysis.Pass, executors map[string]bool) []LoopInfo {
	var loops []LoopInfo
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch loop := n.(type) {
			case *ast.ForStmt:
				if astContainsExecutionMethod(loop.Body, executors) {
					loops = append(loops, LoopInfo{Start: loop.Pos(), End: loop.End()})
				}
			case *ast.RangeStmt:
				if astContainsExecutionMethod(loop.Body, executors) {
					loops = append(loops, LoopInfo{Start: loop.Pos(), End: loop.End()})
				}
			}
			return true
		})
	}

	return loops
}

func GetGormSinkArgs(call ssa.CallInstruction) []ssa.Value {
	methodName := getCallNameFromInstr(call)
	switch methodName {
	case "Where", "Raw", "Not", "Or", "Select", "Having", "Group", "Order", "Query", "QueryRow", "Exec":
		if len(call.Common().Args) > 1 {
			return call.Common().Args[1:]
		}
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

func ComputeExecutorSet(funcs []*ssa.Function, pass *analysis.Pass) map[*ssa.Function]bool {
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

func BuildTransitiveExecutors(funcs []*ssa.Function, pass *analysis.Pass) map[string]bool {
	execMap := ComputeExecutorSet(funcs, pass)

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
