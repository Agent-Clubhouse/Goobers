# Componentization extraction timing

This standalone command records a versioned JSON baseline for comparing the
same Go workload before and after component extraction. It runs, sequentially,
selected tests, no-selected-tests package loading, and a full binary build.
Each workload runs in normal and race modes against cold and warm build caches.
Warm caches are primed once before measurement; every cold sample gets a newly
created `GOCACHE`. Test commands always use `-count=1`.

`-run '^$'` measures package loading and test-binary startup with no selected
tests. It is not pure compiler time: package `TestMain` functions still run.
Likewise, an isolated `GOCACHE` does not make the module download cache or OS
filesystem cache cold. Control those separately when the experiment requires
it.

## Invocation

Use a private, local, non-synced cache root. On Windows, a suitable default is
under `%LOCALAPPDATA%`; do not put caches or runtime state under OneDrive.

```powershell
$revision = git rev-parse HEAD
go run ./test/componentizationbaseline/timing `
  -checkout . `
  -revision $revision `
  -package ./internal/runner `
  -package ./internal/workflow `
  -test '^TestCompile$' `
  -test '^TestAdvance$' `
  -repeats 7 `
  -tags integration `
  -cgo 1 `
  -build ./cmd/goobers `
  -cache-root "$env:LOCALAPPDATA\Goobers\componentization-timing" `
  -out .\timing.json
```

`-package` and `-test` are repeatable. The command requires the checkout's
exact `HEAD` revision and records whether the checkout is dirty. It also
records Go version, compiler, OS, architecture, tags, CGO setting, exact
argument vectors, every duration and exit status, sample counts, and
min/median/mean/p95/max statistics. The default is five samples per workload;
use at least five successful samples per workload for comparisons (lower
configured counts are for exploratory runs only). Command failures remain in
the report, are excluded from successful-sample statistics, and make the
command exit nonzero after all workloads finish.
Cancellation writes the partial report and exits nonzero.

The output follows [`schema.json`](schema.json). The tool creates one uniquely
named workspace below `-cache-root`, removes only that workspace, and preserves
the named evidence output and cache root. Evidence contains no command output,
environment dump, credentials, or authentication state.

## Comparable results

Compare reports only when revision state, package and test selectors, build
target, Go/compiler/OS/architecture, tags, CGO, normal/race mode, cache state,
and sample count match. Run on otherwise idle machines of the same hardware
class and power policy. Record machine identity externally if needed; it is
deliberately absent from evidence to avoid leaking host or user data.

Wall-clock measurements remain sensitive to scheduler activity, antivirus,
thermal throttling, background I/O, module-download state, and OS filesystem
caches. Prefer more samples and repeated runs over interpreting small
differences. Do not compare a failed sample to a successful sample as if they
measured the same work.
