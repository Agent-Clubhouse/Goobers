# Control-plane extraction CI gates

The control-plane HA extraction moves reusable code from `cmd/goobers`
(`package main`) into `internal/controlplane/...` before `goobers-cloud` is
wired. A move must not look like new complexity or command growth, but it also
must not receive fresh headroom or escape coverage and dead-code review.

These rules apply to every extraction PR, beginning with #7716.

## Required shape

An extraction PR is either a pure move or an ordinary code change:

- A **pure move** preserves the complete function declaration byte-for-byte,
  including signature, body, comments inside the declaration, complexity, and
  body length. Package declarations, imports, callers, and tests may change.
- Any edit within the function declaration is an **ordinary code change**. It
  uses the existing complexity and coverage rules even when `git diff` labels
  the file as a rename.
- New stores and packages are wired into the same PR's role constructor or
  explicit skeleton root. Extraction is not permission to land unreachable
  production code.

## Deadcode

`test/deadcode/exemptions.txt` is keyed by the analyzer's exact
`module/package.symbol` identity. Moving a listed symbol makes the old entry
stale and the new identity unreviewed.

1. Run `make deadcode` after wiring the moved package.
2. Delete the old exemption when the symbol becomes reachable or disappears.
3. If the same compatibility surface remains intentionally unreachable, replace
   the old package-qualified identity in the same PR. Preserve its platform
   qualifier and rationale, and make the replacement satisfy the current
   exemption policy with an open owning issue. Do not add an expiry merely to
   get an extraction merged.
4. Never exempt a newly introduced store, constructor, or adapter. Wire it.

The analyzer's stale-entry and per-platform checks remain authoritative; there
is no automatic deadcode ledger rewrite because declaration identity does not
prove reachability intent.

## Complexity and body length

The complexity baseline is keyed by path plus symbol. A plain move therefore
looks like a stale old row and a new over-cap function. Use the move-only
updater in the uncommitted extraction worktree:

```sh
make complexity-move-update
go run ./test/complexitygate
```

The updater reads the old declaration from `HEAD` and relocates complexity and
body-length rows only when exactly one current function has the same symbol and
a byte-identical declaration hash. It preserves the old ceilings,
justifications, ratchet budget, and fixed body-length cap. It refuses:

- an edited signature or body, even if cyclomatic complexity and line count are
  unchanged;
- an ambiguous duplicate declaration;
- hand-edited baseline values;
- a new over-cap function or any increase caught by normal enforcement.

Use `make complexity-update` only for a real decomposition or separately
justified ordinary change. A move cannot reset a historical ceiling to its
current measurement or consume a ratchet-budget increase.

## Coverage

The coverage rule has **zero extraction-specific headroom**:

- `test/coveragegate` excludes `/cmd/` wrappers but includes every
  `internal/controlplane/...` file. Do not add an extraction path to
  `COVERAGE_EXCLUDE`.
- Move or add package-local tests with the logic. Tests left only in
  `cmd/goobers` do not establish that the new internal package remains
  independently testable.
- Preserve behavioral tests at the command seam as wiring/equivalence coverage;
  they complement rather than replace the internal unit tests.
- PostgreSQL and object-storage implementations added after extraction must
  keep pure decisions behind fakes and unit-test them in the default suite.
  Tests requiring PostgreSQL, Azurite, or a network fault proxy use
  `//go:build integration` and the declared CI services. Integration-only
  assertions do not buy a lower global threshold.

The only budget is the repository-wide `COVERAGE_THRESHOLD`. If moving formerly
excluded command logic into the measured internal denominator drops the total,
the extraction adds tests rather than changing the denominator or threshold.

## Command growth

The all-command ratchet introduced by #7727 evaluates each direct `cmd/*`
`package main` independently against the PR merge-base.

- Lines removed from `cmd/goobers` are credited immediately as that command's
  negative delta. No baseline update or justification is needed.
- That decrease is **not transferable**. It cannot offset growth in
  `cmd/goobers-cloud`, `cmd/operator`, or another command.
- Internal package lines are not command growth; they are governed by
  complexity, coverage, lint, and deadcode instead.
- A command skeleton or changed wrapper that grows still needs its own newly
  added growth justification. #7723 owns the first `goobers-cloud` skeleton and
  its command baseline.

## Extraction PR checklist

Run with the repository-pinned Go toolchain:

```sh
make complexity-move-update # only when a baselined declaration moved unchanged
make deadcode
go run ./test/complexitygate
go test ./test/coveragegate ./test/cmdgoobersgrowth
go run ./test/cmdgoobersgrowth -base-ref origin/main -head-ref HEAD
go test -race ./<moved-package> ./cmd/goobers
```

Review the diff after every updater. It should change only old/new baseline keys
for pure moves; scores, ceilings, budgets, and unrelated rows must stay fixed.
