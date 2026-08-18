# Query Controller

This project is based on the [Static Program Analysis](https://cs.au.dk/~amoeller/spa/spa.pdf) book.

The goal of this project is to find the N+1 query problem by tracing whether data used in the ORM leads to a loop, to be sure that it is a true N+1 query problem. 

Implementation details are written in [onboarding.md](onboarding.md).

## How it works

Analysis runs in two phases:

1. **Phase 1 — find queries**
   First, we detect where queries execute.


2. **Phase 2 — Vulnerability check**
   Then, we trace query parameters to check whether they come from a loop source.

## golangci-lint plugin

The plugin registers as the linter name **`nplusone`**.

### Build a custom golangci-lint binary

Create a `.custom-gcl.yml` (see `.custom-gcl.yml` in the repo root) and build the custom binary:

```bash
golangci-lint custom
```

This produces a `custom-gcl` binary in the current directory.
