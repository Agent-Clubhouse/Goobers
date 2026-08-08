# DX/docs debt batch: tracked mutation-sidecar test artifact, and undocumented ci-poll / headSha-baseSha / agent:model / spec.skills authoring contracts

Suggested labels: area:hygiene, documentation, type:bug

## Problem

Five small, independently-fixable defects, batched because each is a
first-hour authoring trap the cold-start audit hit *by accident* rather than
by design, and none needs its own design discussion:

**(a) `cmd/goobers/mutations.jsonl` is a tracked file that ordinary test runs
mutate in place.** `sidecarMutationRecorder.RecordExternalRef` appends to a
bare, CWD-relative path with no directory injection. In production this is
correct — the runner always `cd`s into the stage's worktree before invoking
`goobers open-pr`/`backlog-query`/etc. as a subprocess. In tests, any code
path that reaches this recorder without first isolating the working
directory (`t.Chdir(t.TempDir())`) writes into the package source directory
instead, dirtying the tree and leaving a growing file that has already
reached `main` once by accident.

**(b) `ci-poll` has no authoring surface anywhere in shipped docs.** It is a
GA, first-class built-in stage kind central to the flagship `implementation`
workflow, but `goobers help stages` omits it, `goobers ci-poll --help`
prints "unknown command", there is no man page, and its entire contract
(`inputs.kind: "ci-poll"`, its `run.command` being a required-but-inert
placeholder, its `prNumber` input, its `ciStatus` output) exists only as
YAML comments inside one shipped example file.

**(c) Nothing documents which stage produces `headSha`/`baseSha`.**
`merge-pr --help` states they are required inputs and that `verdictAuthor`
is "supplied by apply-verdict", but says nothing about where the two SHAs
come from. The only way to learn `pr-select` is the sole producer is to
grep `config-examples/`.

**(d) `agent:model` is missing from essentially every shipped example
goober**, despite the scaffolder's own generated comment and the onboarding
guide both stating it is required for the Copilot harness to receive its
model credential — so a new author who copies a shipped reference verbatim
gets a goober that cannot authenticate, with `validate` silent either way.

**(e) `Goober.spec.skills`' package format is completely undocumented, and
the only check that exists is existence-only** — an empty directory
(`mkdir`) satisfies `--strict`'s SKILL002 gate exactly as well as a real
skill package, so the one enforcement point for this field can be defeated
trivially and gives no signal that anything was authored correctly.

## Evidence

**(a)**
- `cmd/goobers/mutationsidecar.go:13-23,61-82` — `mutationsSidecarFile =
  "mutations.jsonl"`, opened via `os.OpenFile(mutationsSidecarFile,
  os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)`: a bare relative name, no
  directory parameter, no override seam.
- `cmd/goobers/mutationsidecar_test.go:48-99` — the package's own intentional
  tests of this exact path (`TestOpenPRWritesMutationSidecar`,
  `TestBacklogQueryClaimWritesMutationSidecar`) each manually isolate via
  `workDir := t.TempDir(); t.Chdir(workDir)` before invoking the command —
  proving the required discipline is known, but it lives in each test
  author's memory, not in any shared helper or guard.
- `git log --follow -p -- cmd/goobers/mutations.jsonl`: the file was created
  by a single stray line committed alongside an unrelated PR (`fix(starter):
  default-implement now delivers the promised first PR (#2447) (#2552)`,
  commit `2763ec23`, 2026-08-07) that never touched any mutation-sidecar
  code — direct proof this happens during ordinary local development
  (build, test, `git add`), not only in a hypothetical.
- Live during this audit session (2026-08-08, this worktree): the tracked
  file grew from the single committed line to 6 lines across a handful of
  `go test ./cmd/goobers/...` invocations run for this investigation, with
  no test in this package intentionally targeting it — consistent with
  either an unisolated test elsewhere in the ~40 files that exercise
  mutation-recording commands, or two concurrent `go test` processes
  against the same worktree racing on the same relative path (this session
  observed exactly that: a second, independent `go test -race
  -timeout 30m ./...` process with `cwd` set to this same worktree, running
  concurrently). Either mechanism confirms the same defect: the file is an
  unisolated, shared, git-tracked mutable resource.
- `.gitignore`'s own comments record this repo hitting the identical *class*
  of incident twice already for build binaries ("a 33MB `goobers` binary
  reached main this way", "a 37MB binary reached a PR this way, #1114") —
  each fixed by a targeted `.gitignore` entry, never a directory-isolation
  fix, because the underlying cause (an un-isolated relative path) was never
  addressed structurally.

**(b)**
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-minimal.md`
  "Docs notes": "The biggest gap is `ci-poll`. ... `goobers help stages`
  lists ~35 stages and omits it, `goobers ci-poll --help` prints `goobers:
  unknown command "ci-poll"` ..., there is no docs/man/goobers-ci-poll.1,
  and docs/cli/README.md mentions it only obliquely inside the
  *merge-queue-poll* entry. ... exists only as YAML comments inside
  config-examples/gaggles/acme-web/workflows/implementation.yaml. Delete
  that one example file and `ci-poll` becomes unauthorable from shipped
  docs."
- `cmd/goobers/validate.go:722-730` (`stageCommandProblems`) — the code's
  own comment confirms the mechanism: "A stage that declares a built-in
  kind (e.g. kind=ci-poll) is dispatched to a dedicated executor and its
  command is an inert placeholder ... which is not a CLI verb — so it must
  not be surface-checked here" — i.e., `ci-poll` is structurally excluded
  from the CLI command registry that `help stages`/man pages are generated
  from (per the registry-sourced docs/help convention), so it has no path
  into either.
- `internal/executor/dispatch.go:20-24,120-163` — the real contract
  (`KindCIPoll`, `NewCIPollKindExecutor`, `CIPollConfigFromEnvelope`) lives
  entirely in Go, with no doc surface pointing at it.

**(c)**
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-minimal.md`
  "Docs notes", second paragraph: "nothing documents which stages *produce*
  `headSha`/`baseSha`. `merge-pr --help` says they are required and that
  `verdictAuthor` is 'supplied by apply-verdict', but never says where the
  SHAs come from. I had to grep config-examples/ to discover `pr-select` is
  the sole producer."
- `docs/man/goobers-merge-pr.1:15-18`: "Declared inputs: pullNumber,
  verdict, headSha, baseSha (all required), verdictAuthor (required for the
  default commit message; supplied by apply-verdict)" — names the producer
  for one input, silent on the other two.

**(d)**
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-python.md`
  ledger #3: "Not one shipped example goober declares `agent:model`. But
  `goobers scaffold goober curator` emits it with the comment '# Required
  for the Copilot harness to receive its model credential', and
  arbitrary-repo-onboarding.md §2 says the Copilot token 'is injected as
  COPILOT_GITHUB_TOKEN only into agentic subprocesses that declare
  `agent:model`'." (Precision check against current `main` for this draft,
  2026-08-08: 11 of the 12 `config-examples/gaggles/*/goobers/*/goober.yaml`
  files omit it; the one exception, `acme-web`'s `docs` goober, declares it.
  All 12 flagship curator/implementer/reviewer goobers across all four
  reference stacks — acme-web, python-service, dotnet-service, java-service
  — and both `internal/instance/starter` and
  `internal/instance/quickstart-v1`'s scaffolded goobers omit it.)
- `cmd/goobers/templates/scaffold/goober.yaml.tmpl:15` and
  `docs/guides/arbitrary-repo-onboarding.md:87` both state the requirement;
  `validate` has no rule cross-checking a goober's `harness: copilot` +
  agentic workflow membership against a missing `agent:model` capability.

**(e)**
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-python.md`
  ledger #4 and "Docs notes": "Nothing anywhere documents the skill-package
  format: `goobers explain --human Goober.spec.skills` says only 'Named
  skills available to this goober'; docs/requirements/goober.md mentions
  skills in a one-line table row; the goobers-dsl-author DSL reference
  mentions `skills` once inside a list with no explanation; no guide covers
  it. ... an EMPTY directory also clears it — the check is existence-only,
  so --strict gating on SKILL002 can be satisfied with `mkdir`."
- `api/schemas/goober.schema.json:154` — the sole "documentation":
  `"description": "Named skills available to this goober."`
- `docs/guides/dsl-authoring-skill.md:6-7` explicitly disclaims coverage:
  the agent toolkit's own skills are "distinct from skills configured on
  workflow goobers" — the one guide that mentions the word `skills` states
  outright that it is *not* about this field.
- `api/validate/validate.go:973-990` (`checkMissingSkillPackages`) — the
  check is `os.Stat(scoped)` / `os.Stat(shared)` gated only on
  `IsDir()`; no check for a manifest file, frontmatter, or any content
  inside the directory.

## Proposed direction

**(a)** `git rm --cached cmd/goobers/mutations.jsonl`; add
`/cmd/goobers/mutations.jsonl` to `.gitignore` (matching this repo's
existing per-path convention for stray build/test artifacts, not a broad
`*.jsonl` rule that could hide a real fixture elsewhere). Then close the gap
structurally rather than just removing today's instance: add a
`TestMain`-level or dedicated suite-wide guard test (same pattern as
`TestGitFsyncDisabledForSuite`/`TestJournalFsyncDisabledForSuite` in
`cmd/goobers/testmain_test.go`) that fails loudly if
`cmd/goobers/mutations.jsonl` exists or is non-empty after the package's
tests run — this converts a silent, hard-to-attribute tree-dirtying bug into
a fast, named test failure the next time any test regresses the isolation
discipline, and would have caught this session's regrowth immediately
instead of requiring manual `git diff` archaeology.

**(b)** Ship a `docs/man/goobers-ci-poll.1`-equivalent authoring reference
even though `ci-poll` is not a CLI verb: document its `inputs.kind`
requirement, its inert `run.command` placeholder, its `inputsFrom`
requirement (`prNumber`), and its `ciStatus` output directly in the field
descriptions the schema already carries for `TaskDeterministic`'s `kind`
enum, and add it to `docs/cli/README.md`'s stage reference rather than only
the oblique `merge-queue-poll` mention. `goobers help stages` should list
built-in *stage kinds* (ci-poll included) alongside CLI-verb stages, even
though they are dispatched differently, since an author authoring a
workflow does not care which internal mechanism executes a stage — only
that it is discoverable.

**(c)** Add one sentence to `docs/man/goobers-merge-pr.1` and its source
help text naming `pr-select` as the producer of `headSha`/`baseSets`
(mirroring the existing "supplied by apply-verdict" phrasing already used
for `verdictAuthor` on the same line) — a one-line, zero-risk documentation
fix.

**(d)** Add `agent:model` to every shipped agentic reference goober across
`config-examples/`, `internal/instance/starter`, and
`internal/instance/quickstart-v1` wherever `harness: copilot` (or any
harness requiring model credentials) is declared, so a verbatim copy of a
shipped example authenticates out of the box — matching what the
scaffolder and the onboarding guide already promise. Separately, add a
`validate` warning when a goober declares an agentic harness and belongs to
a workflow with agentic tasks but does not declare `agent:model`, closing
the detection gap alongside the docs fix (a schema-only fix would leave
future examples free to regress silently again).

**(e)** Document the skill-package format explicitly — minimally, point at
the shipped agent-toolkit layout (`SKILL.md` with name/description
frontmatter) as the sanctioned shape, in both
`docs/requirements/goober.md` and the schema description validate/`explain`
surface. Strengthen SKILL002 from existence-only to requiring at minimum a
non-empty `SKILL.md` (or equivalent manifest) inside the declared
directory, so the check gates on the thing an author is actually supposed
to produce, not just a directory node existing.

## Alternatives considered

- **(a) Add a broad `*.jsonl` `.gitignore` rule instead of the specific
  path.** Rejected: too coarse for a repo that may legitimately want to
  track a `.jsonl` fixture elsewhere (golden test data, for instance); the
  targeted per-path entry matches this repo's own established convention
  for exactly this incident class.
- **(a) Skip the suite-wide guard test, rely on `.gitignore` alone.**
  Rejected as incomplete: `.gitignore` stops the file from being *tracked*
  again, but does not stop tests from silently writing into the live source
  tree during local development (a developer's `git status` still shows
  nothing new, but any tool relying on a clean working tree — `make
  fix`, a diff-based lint, a stray `git add -A` — remains exposed to the
  same class of accidental capture that already happened once).
- **(b) Give `ci-poll` a real CLI subcommand purely so it appears in the
  registry.** Rejected: it is deliberately dispatched by kind, not by
  subcommand, and the schema's own placeholder-command design is
  intentional (per `validate.go`'s comment); inventing a subcommand that
  never actually runs would be more confusing than documenting the kind
  directly.
- **(e) Require a specific frontmatter schema for `SKILL.md`, validated
  field-by-field.** Rejected as first step: a non-empty-manifest check
  closes the "mkdir defeats --strict" hole with a one-line change; stricter
  frontmatter validation can be layered on later without blocking this fix.

## Duplicate search

2026-08-08, `gh issue list -R Agent-Clubhouse/Goobers --search "<term>"
--state all`, terms per item:
- (a): `mutations.jsonl`, `test appends tracked file`, `cmd/goobers test
  debris`, `mutation sidecar cwd`, `tracked file test writes`, `gitignore
  test artifact`. Nearest hit: **#228** (closed) — shipped the mutation
  sidecar mechanism itself; no relation to its test-isolation defect.
  **#1736** (closed) — same *class* of incident (stray build artifact
  committed for want of a `.gitignore` rule, "same class of gap as the
  earlier committed-binary incident") but for a Vitest cache file, not this
  path; confirms the fix pattern this draft proposes, does not cover this
  file.
- (b): `ci-poll documentation`, `ci-poll man page help stages`, `ci-poll
  unauthorable`. No hits naming `ci-poll`'s documentation gap anywhere,
  open or closed.
- (c): `headSha baseSha`, `merge-pr headSha baseSha undocumented`,
  `pr-select sole producer sha`. Numerous hits, all about merge-review
  caching/escalation mechanics (#718, #786, #1733, #1052, #2378, #832,
  #747, #748) — none about `headSha`/`baseSha`'s *producer* being
  undocumented; the field itself is a settled, working mechanism in all of
  them.
- (d): `agent:model example`. Hits: **#290** (closed, docs: add
  `agent:model` row to a token-scopes guide — a different doc), **#566**
  (closed, tutor goobers specifically lacking `agent:model` — a narrower,
  already-fixed instance of the same class, not the shipped
  config-examples reference set), **#576/#578** (closed, scaffold/tutorial
  issues, don't audit shipped examples for this field). None covers the
  current state of the 12 `config-examples` reference goobers.
- (e): `spec.skills package`, `SKILL002`, `skill package format
  undocumented`, `skill directory existence check`, `explain skills
  tautology`. No hits beyond this audit's own coldstart files (which are
  not upstream issues).

## Size and risk

**S**, all five items. (a) is a `.gitignore` entry + `git rm --cached` +
one new guard test, no production code change. (b)/(c)/(d)/(e) are docs-
and-schema-description edits plus one narrow `validate` rule each ((d)'s
harness/agentic-membership warning, (e)'s manifest-presence check); none
changes runtime behavior for a config that already validates clean today.
Recommend filing (a) as its own issue given its distinct root cause and
owner (test infra vs. docs); (b)-(e) can stay batched as a single docs-debt
pass since each is a one-file, low-risk edit.
