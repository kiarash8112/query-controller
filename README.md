# Query Controller

Static analyzer for detecting **true, data-dependent N+1 database query** patterns in Go code. The tool traces query parameters backward from GORM-style sinks through interprocedural dataflow and reports cases where a dynamic value reaches a database call inside a loop.

Unlike pattern matchers that flag any query inside a loop, Query Controller uses IFDS-style tabulation to distinguish real N+1 issues (loop-indexed or phi-dependent values flowing into query arguments) from safe constant queries.

## How it works

Analysis runs in two phases:

1. **Phase 1 — Dataflow trace**  
   Seeds GORM query sinks (`Where`, `Raw`, `Query`, `Exec`, etc.) and walks backward through SSA, following assignments, stores, and cross-function parameter/return edges.

2. **Phase 2 — Vulnerability check**  
   Flags sinks inside loops that also invoke a database **execution** method (`Find`, `Scan`, `First`, `Exec`, …) when the traced argument is not a compile-time constant and depends on loop iteration (phi nodes, indexed access, etc.).

```
  for _, u := range users {
      GetUser(db, u.name)   // loop variable flows into query arg
  }

  func GetUser(db *GormDB, u string) {
      db.Where("it is", u).Find(nil)   // ← reported as N+1
  }
```

## Repository layout

| Path | Description |
|------|-------------|
| `ifds.go`, `helper.go`, `ast.go`, `model.go` | Standalone CLI analyzer (stack-based IFDS) |
| `examples/` | Sample Go packages used by the CLI |
| `analyzerVersion/` | Production **golangci-lint plugin** with cross-package fact propagation |
| `analyzerVersion/code_examples/` | Test fixtures for the plugin |

The root package is a research/prototype CLI. The `analyzerVersion/` directory is the maintained linter implementation with cross-package summaries (`SinkParamFact`, `ReturnToParamFact`, `ExecutorFact`) and golangci-lint integration.

## Requirements

- Go 1.23+ (root CLI)
- Go 1.25+ (`analyzerVersion/` plugin and tests)
- [golangci-lint v2](https://golangci-lint.run/) with [custom build support](https://golangci-lint.run/docs/plugins/module-plugins/) (for the plugin)

## Standalone CLI

Analyze a Go module directory (defaults to `examples/`):

```bash
go run . examples/example1
```

Pass a different target path as the first argument:

```bash
go run . /path/to/your/module
```

On success the tool prints a vulnerability report to stdout. A clean project prints:

```
✅ Project Clean! No N+1 Queries detected.
```

## golangci-lint plugin

The plugin registers as linter name **`nplusone`**.

### Build a custom golangci-lint binary

1. Edit `analyzerVersion/.custom-gcl.yml` and set `path` to your local `analyzerVersion` directory.
2. Build the custom binary:

```bash
cd analyzerVersion
golangci-lint custom
```

This produces a `custom-gcl` binary in the current directory.

### Run the linter

```bash
./custom-gcl run ./...
```

Configuration lives in `analyzerVersion/.golangci.yml`. Example diagnostic:

```
🚨 [TRUE N+1] Found dynamic database execution in loop (detected via dataflow)
```

## Tests

Run plugin tests from the `analyzerVersion` directory:

```bash
cd analyzerVersion
go test ./...
```

Tests cover scenarios such as base N+1, nested functions, tuple returns, recursion, dynamic query building, and state checking. Fixtures live under `analyzerVersion/code_examples/`.

## Detected APIs

**Query sinks** (arguments are traced):

`Where`, `Raw`, `Not`, `Or`, `Select`, `Having`, `Group`, `Order`, `Query`, `QueryRow`, `Exec`

**Execution methods** (must appear in the loop body for a finding):

`Scan`, `Find`, `First`, `Take`, `Last`, `Pluck`, `Count`, `Exec`, `QueryRow`, `Query`

The standalone CLI also treats `print` as an execution method for debugging.

## Limitations

- Targets GORM-style method names; other ORMs are not modeled unless they use the same surface API.
- Cross-package analysis is fully supported only in the `analyzerVersion` plugin (via exported analysis facts).
- The standalone CLI uses a call-stack depth limit (`MaxCallStackDepth = 7`) for interprocedural tracing.
- Findings require data dependence on loop iteration; queries in loops with constant arguments are not reported.

## License

See repository history for license information.
