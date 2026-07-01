// Package plugin registers the nplusone analyzer as a golangci-lint module plugin.
package plugin

import (
	"encoding/gob"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"

	linters "github.com/kiarash8112/query-controller"
	sinks "github.com/kiarash8112/query-controller/internal/sink_finding"
	tracefunc "github.com/kiarash8112/query-controller/internal/trace_function"
)

func init() {
	register.Plugin("nplusone", New)
	gob.Register(&sinks.SinkParamFact{})
	gob.Register(&tracefunc.ReturnToParamFact{})
	gob.Register(&sinks.ExecutorFact{})
}

type nplusonePlugin struct{}

func New(_ any) (register.LinterPlugin, error) {
	return &nplusonePlugin{}, nil
}

func (p *nplusonePlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{linters.Analyzer}, nil
}

func (p *nplusonePlugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}
