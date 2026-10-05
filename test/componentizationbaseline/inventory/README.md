# Componentization baseline inventory

This standalone command records the current `cmd/goobers` command boundary and
the tests relevant to extracting docs-churn, contention, and PR-status. It only
reads Go, Git, and source metadata; it does not change production behavior.

Run it from the repository root:

```text
go run ./test/componentizationbaseline/inventory > inventory.json
```

Use `-goos`, `-goarch`, and `-tags tag1,tag2` to inventory another build
context, or `-output path` to write directly to a file. The command fails
without publishing output when Git or Go discovery fails or returns malformed
metadata.

## Schema

`schemaVersion` is
`goobers.dev/componentization-baseline/inventory/v1`. `source` records the Git
commit and module path discovered by `go list -m`; `buildContext` records the Go
toolchain, GOOS, GOARCH, CGO setting, and tags. `command` contains the local
production and test dependency closures, test-only dependencies, direct and
test imports, and source/test/function counts. `packages` supplies the same
classification per repository-local package, including files excluded by the
selected build context.

`tests` classifies each test, benchmark, and example. Domain references come
from AST references to declarations owned by `docchurn.go`,
`contestedfiles.go`, and `reportprstatus.go`; records also identify tests that
straddle those domains, access environment APIs, or reference package-level
function factories. `domains` lists the source and test ownership and existing
same-package helpers and repository-local imports useful to each domain, rather
than proposing replacement utilities. `omissions` explicitly states that one
invocation covers one build context.

All collections and build tags are sorted, paths are repository-relative with
slash separators, and the output contains no timestamp, absolute path, or Go
cache identity. The same source revision, toolchain, and build context therefore
produce byte-identical JSON.
