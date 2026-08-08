# `goobers init` has no non-interactive way to select the standard backlog-curation + implementation workflow pair

Suggested labels: area:workflows, enhancement, area:contracts

## Problem

The flagship onboarding path — backlog-curation feeding implementation, the
pair every quickstart-adjacent guide treats as "the standard setup" — has
exactly one way to scaffold it: `init --guided`'s interactive workflow-
selection prompt. A script, an agent, or any caller without a terminal
attached has no route to it. `init --template` exists but only accepts
`quickstart`, which is documented as intentionally not production-safe.
The practical consequence: a non-interactive setup has to delete the bare
`init` scaffold's `coder` goober and `default-implement` workflow, then
hand-assemble three goober directories and two ~200-400 line workflow files
from `goobers examples show` output — the exact ceremony `examples show`
and `connect` otherwise eliminate for every other part of onboarding.

## Evidence

- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-python.md`
  ledger #2 (dsl-ceremony / via cli-help): "Bare init only scaffolds one
  manual-only `default-implement` workflow and one `coder`. ... guided is
  interactive-only, so from a script/agent there is no path to the standard
  pair. `init --template=quickstart` exists but is documented as 'not for
  production'. Everything had to be assembled by hand from `goobers
  examples show`." Cost: 186 lines of YAML authored for this flavor alone,
  10 tweaks, only 1 caught by `validate`.
- Same file's "DSL ceremony notes": "there is no non-interactive route to
  the standard curation+implementation pair: `init --guided` can select
  both, but only through prompts, so a scripted setup must delete the
  scaffolded `coder`/`default-implement`, hand-create three goober
  directories, and copy two 200-400 line workflows. A `--template=standard`
  or `init --workflows=backlog-curation,implementation` would erase most of
  this exercise."
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/README.md` cross-
  cutting finding #3: "no non-interactive route to the standard pair
  (guided is prompt-only)" — independently confirmed as a systemic
  scaffold-ladder gap, not a single flavor's artifact.
- `cmd/goobers/init.go:27-53` (`initHelp`) — the full documented flag
  surface: `--guided`, `--demo`, `--template=quickstart[--source-tree]`.
  No workflow-selection flag exists outside `--guided`.
- `cmd/goobers/init.go:409-424` — workflow selection lives entirely inside
  the interactive prompt: `p.ask("Select workflows ...")` →
  `parseWorkflowSelection(workflowText)`, gated behind the `guidedPrompter`
  interface with no non-interactive entry point into the same selection
  logic.
- This branch's own `cmd/goobers/scaffoldgaggle.go` (uncommitted, this PR)
  adds `goobers scaffold gaggle --from <existing>` — it closes the
  *rename* half of the scaffold-ladder gap the same cold-start exercise
  found (swift #1 / dotnet #9: "there is no `goobers scaffold gaggle` and
  no rename path"). Its own doc comment scopes it explicitly to identity
  fields (`metadata.name`, `isolation.namespace`, manifest registration)
  and states it leaves `ciCommand`, instructions, and workflow bodies
  untouched — it does not scaffold or select workflows, so this proposal's
  delta is fully uncovered by it.

## Proposed direction

Add `--workflows=<comma-separated names or numbers>` to `goobers init`,
accepting exactly the values `init --guided`'s prompt already validates
(`instance.GuidedWorkflowNames()` / `parseWorkflowSelection`). When passed
without `--guided`, run the same option-building path guided uses
(`buildGuidedOptions`, minus the interactive `ask` calls it currently
requires) with the selection supplied by the flag instead of a prompt —
same generated goobers, same credential/policy-action wiring per selected
workflow, same "Next:" output. This keeps one implementation of "what does
selecting backlog-curation + implementation actually produce" instead of a
second, quietly-diverging non-interactive path.

Zero-config default is unchanged: bare `init` (no flags at all) keeps
scaffolding today's single manual-only `default-implement` + `coder` pair —
the safest possible first `goobers up`, which the audit elsewhere confirms
is a deliberate, documented safety property. `--workflows` is opt-in
progressive disclosure: a caller who wants the flagship pair states it
explicitly and gets it in one command; everyone else's behavior is
byte-identical to today.

## Alternatives considered

- **A second named template, `--template=standard`.** Rejected as the
  primary mechanism: `--template` today seeds a *static* file tree
  (`--source-tree`'s own doc: "seeds the checked-in source layout ...
  without runtime state"); the standard pair needs live inputs (repo
  owner/name, credential mapping, CI-command detection) that only guided's
  option-builder already resolves. A static `standard` template would
  either re-implement that resolution or ship placeholder-laden YAML that
  immediately needs the same hand-editing `connect` exists to eliminate —
  doubling the maintenance surface for one already-solved code path.
- **A fourth CLI onboarding action in the #1468 family** (alongside
  seed-starter-config-source, stub-sample-target-repo, stub-instruction-
  assets — all closed, all shipped). Plausible alternative shape (the
  family's contract — non-interactive, `--json` envelope, idempotent — is
  a good fit), but workflow selection's actual logic is already threaded
  through `promptGuidedOptions`/`buildGuidedOptions` inside `init`, not a
  standalone action; a flag on `init --guided` is the smaller diff and
  keeps the family's three existing actions (config-source, sample-target,
  instructions) orthogonal to workflow choice rather than adding a fourth
  that overlaps `init`'s own responsibility.

## Duplicate search

2026-08-08, `gh issue list -R Agent-Clubhouse/Goobers --search "<term>"
--state all`, terms: `non-interactive init workflows`, `init template
standard`, `guided prompt only scripted`, `init --workflows`, `guided
non-interactive`, `scaffold gaggle rename`, `standard curation
implementation pair scaffold`, `template quickstart production`,
`hand-assemble workflows`.

Nearest hits, all non-covering:
- **#1691, #1690, #1692** (all closed, all parented to #1468) — non-
  interactive CLI onboarding actions, but for three specifically different
  steps: seeding a starter config-source tree, stubbing a disposable sample
  target repo, and stubbing agent-instruction assets. None selects *which
  canonical workflows* get scaffolded; #1691's own body scopes it as
  "Extends the existing `init --template` path" for tree-seeding only.
- **#1468** (closed) — establishes the non-interactive action *contract*
  (`--json` envelope, idempotent, no prompts) that a future `--workflows`
  flag should follow, but its own summary enumerates exactly the same three
  actions as #1691/#1690/#1692 — workflow selection is not among them.
- **#435** (open, epic) — "Onboarding & Authoring", the parent epic whose
  scope line reads "choose which canonical workflows to configure" as part
  of guided first-run (#436, closed) — confirms this is a recognized need,
  but only as an *interactive* guided-first-run item; no open child covers
  a non-interactive equivalent.
- **#2173/#2071** (closed) — about `init`'s default `ciCommand` value and
  guided's stale-command scaffolding bug, unrelated to workflow selection.
- Nothing found combining "init", "non-interactive", and "workflow
  selection"/"standard pair" as a single ask.

## Size and risk

**S.** Additive CLI flag reusing existing, already-tested selection and
option-building logic (`parseWorkflowSelection`, `buildGuidedOptions`); no
schema change, no change to any existing invocation's behavior (bare
`init`, `--demo`, `--template=quickstart`, and interactive `--guided` are
all unaffected). Main implementation work is threading a non-interactive
value source into `buildGuidedOptions` in place of the `guidedPrompter`
for the fields workflow selection currently prompts for (workflow list,
and any prompt gated on the selected set, e.g. CI-command detection for
`implementation`).
