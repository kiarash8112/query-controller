package linters

import (
	"encoding/gob"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

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
