package linters_test

import (
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/kiarash8112/querycontrolleranalyzer/plugin"
	"github.com/golangci/plugin-module-register/register"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestBaseScenario(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "base_scenario/code.go")
}

func TestDynamicBuild(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "dynamic_build/code.go")
}

func TestNestedFunction(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "nested_function/example.go")
}

func TestStateChecking(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "state_checking/example.go")
}

func TestTupleReturn(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "tuplereturn/code.go")
}

func TestRecursion(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "recursion/code.go")
}

func TestIncreaseI(t *testing.T) {
	newPlugin, err := register.GetPlugin("nplusone")
	require.NoError(t, err)

	plugin, err := newPlugin(nil)
	require.NoError(t, err)

	analyzers, err := plugin.BuildAnalyzers()
	require.NoError(t, err)

	analysistest.Run(t, testdataDir(t), analyzers[0], "increasei/code.go")
}

func testdataDir(t *testing.T) string {
	t.Helper()

	_, testFilename, _, ok := runtime.Caller(1)
	if !ok {
		require.Fail(t, "unable to get current test filename")
	}

	return filepath.Join(filepath.Dir(testFilename), "code_examples")
}
