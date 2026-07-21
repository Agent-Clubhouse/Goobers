# Design: CLI robustness — registry-sourced help + generated man pages

> Status: **Draft for review** · Area: `area:cli` / `DX` · Milestone: **Onboarding & Authoring**
> (#435) References: `cmd/goobers/main.go` (usage), `cmd/goobers/runtime_capabilities.go`
> (registry/dispatch), `cmd/goobers/completion.go` (static completion), `internal/apicontract`.
> Origin: the V1 usability goal — audit CLI commands for robustness, correct/complete man
> pages, and clear core-operation coverage.

## 1. Verdict

**The CLI is functionally broad and its help is real, but there is no single source of truth
for command help — so it drifts and cannot generate documentation.** The command registry
(`var cliCommands` / `cliCommand` struct, `runtime_capabilities.go:12-150`) carries only
`names` + action wiring; **no `Short`/`Long`/`Example` fields.** Help lives in two
disconnected places instead:

- per-command `flag.FlagSet.Usage` closures (32 files), and
- one monolithic top-level `usage()` string (`main.go:41-109`).

Two consequences follow directly:

1. **No man-page / reference generation exists at all** — no `GenManTree`/doc-gen (the CLI is
   hand-rolled stdlib `flag`, not cobra), no `docs/cli/`, no `goobers docs`/`man`. A reference
   cannot be generated because there is no metadata to generate it *from*.
2. **Hand-maintained surfaces have already drifted.** The static completion script
   (`completion.go`, a literal — not registry-derived) is **missing live commands** `blocked`
   (list/clear), `elect-lander`, and `push-remediated`; and `elect-lander`
   (`runtime_capabilities.go:126`) is **absent from the top-level `usage()`** entirely — an
   operator running bare `goobers` never sees it.

Help *density* is otherwise good (most flags are documented inline; exit-code semantics are
stated). The gap is **structure and generation, not missing prose** — plus **zero usage
examples** anywhere and a few **missing core operations**.

## 2. The fix — one metadata source, everything derives from it

### C-1 — Lift help metadata onto the command registry (foundation)
Add `Short`, `Long`, `Examples`, and per-flag descriptions to the `cliCommand` table
(`runtime_capabilities.go:12-18`), and make each command's `fs.Usage` and the top-level
`usage()` **render from that metadata** instead of bespoke strings. This is the load-bearing
change: after it, usage, completion, and man pages all read one source and cannot silently
disagree. Backwards-compatible output (golden-file the rendered help). Every command gets at
least one **usage example** (today there are none — the sprint's most visible DX win).

### C-2 — Generated man pages + CLI reference from the registry
Add a `goobers docs` (or `man`) generator that walks the registry (post-C-1) and emits (a) a
roff man page per command and (b) a Markdown reference under `docs/cli/`, checked in and
verified in CI (regenerate-and-diff, so docs can't drift from the binary — the same discipline
as schema-parity). This is the concrete "man pages" deliverable.

### C-3 — Registry-derived completion + usage; kill the drift
Regenerate `bash`/`zsh`/`fish` completion from the registry (retire the static literal in
`completion.go`), restoring the missing `blocked` / `elect-lander` / `push-remediated` and
guaranteeing future commands appear automatically. Add `elect-lander` to top-level help (falls
out of C-1). A CI check asserts registry ⊇ completion ⊇ usage (no command documented in one
surface but not another) — folds naturally into #650 (CLI-surface validation).

### C-4 — Core-operation coverage audit + gaps
Audit against expected operator verbs; the confirmed gaps and their owners:
- **`config show`** — dump effective/merged config. **No such command today** (`validate` is
  the only config-facing verb). Net-new; small.
- **`version` help** — first-class documented command with `--json` (today a help-thin
  one-liner); coordinate with `goobers versions` (#862) and `goobers features` (#430).
- **Live cancel vs offline abort** — `run abort` (journal edit) exists; live daemon-signaled
  cancel is **#831** (open) — the audit should ensure help "clearly differentiates" them.
- **`doctor`** health/preflight — `--check-harness` is a `validate` flag today; a general
  `doctor` is proposed narrowly in #668 (`--k8s`). Note as a coverage gap; don't build the k8s
  variant here.
- **`logs`** — run output is only via `trace`; note whether a `logs` alias is worth it (likely
  defer — `trace` covers it).

## 3. Decomposition — dispatchable work items

| ID | Issue | Item | Risk | Notes |
|---|---|---|---|---|
| CLI-1 | *(new)* | Lift Short/Long/Examples onto the registry; usage renders from it; add examples | Low-Med | **foundation for C-2/C-3** |
| CLI-2 | *(new)* | `goobers docs`/man generator → roff + `docs/cli/` reference, CI regen-diff | Med | the man-page deliverable |
| CLI-3 | *(new)* | Registry-derived completion + usage; fix drift (blocked/elect-lander/push-remediated); CI parity check | Low | folds into #650 |
| CLI-4 | *(new)* | `config show` + `version --json` + core-op coverage audit | Low-Med | cross-refs #831/#862/#430/#668 |
| — | #650 | CLI-surface validation (shipped stage commands exist + dry-parse) | — | existing guardrail, in scope |
| — | #831 | Live `goobers run cancel` (daemon-signaled) | — | existing, complements CLI-4 |
| — | #439 | Standalone `goobers lint` — one validation path | — | existing, adjacent |

## 4. Recommended sequencing
1. **CLI-1** (registry metadata) — unblocks everything and independently ships examples.
2. **CLI-2** (man/reference gen) and **CLI-3** (completion/usage from registry) — both consume
   CLI-1; can run in parallel.
3. **CLI-4** (core-op gaps) — independent; `config show` is the highest-value net-new verb.
4. Land the CI parity/regen-diff guard (#650 + C-2/C-3 checks) so the surfaces can never
   re-drift.

## 5. Open questions (PO)
- **OQ-1 — `docs`/`man` command name and scope:** ship a `goobers docs` subcommand, or a
  build-time generator only (no runtime command)? *(Recommend: a build-time generator wired
  into CI + committed `docs/cli/`; a thin `goobers docs` wrapper is optional.)*
- **OQ-2 — is `config show` in this sprint** or deferred? *(Recommend: in — it's the one clearly
  missing everyday operator verb and it's small.)*
- **OQ-3 — adopt a CLI framework (cobra) or extend the hand-rolled registry?** *(Recommend:
  extend the registry — C-1 is far less churn than a cobra migration and keeps the existing
  HTTP-parity model in `internal/apicontract` intact; revisit cobra only if man-gen proves
  onerous by hand.)*
</content>
