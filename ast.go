package main

import "go/ast"

func (v *ASTLoopVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}

	isLoop := v.inLoop
	isExecLoop := v.isExecLoop

	switch loopNode := n.(type) {
	case *ast.ForStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
		if isExecLoop {
			*v.LoopRanges = append(*v.LoopRanges, LoopRange{Start: loopNode.Pos(), End: loopNode.End()})
		}
	case *ast.RangeStmt:
		isLoop = true
		isExecLoop = astContainsExecutionMethod(loopNode.Body, v.Executors)
		if isExecLoop {
			*v.LoopRanges = append(*v.LoopRanges, LoopRange{Start: loopNode.Pos(), End: loopNode.End()})
		}
	}

	return &ASTLoopVisitor{
		inLoop:     isLoop,
		isExecLoop: isExecLoop,
		Executors:  v.Executors,
		LoopRanges: v.LoopRanges,
	}
}

func astContainsExecutionMethod(node ast.Node, executors map[string]bool) bool {
	hasExecution := false
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			var name string
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				name = sel.Sel.Name
			} else if ident, ok := call.Fun.(*ast.Ident); ok {
				name = ident.Name
			}

			if isExecutionMethod(name) || executors[name] {
				hasExecution = true
				return false
			}
		}
		return true
	})
	return hasExecution
}
