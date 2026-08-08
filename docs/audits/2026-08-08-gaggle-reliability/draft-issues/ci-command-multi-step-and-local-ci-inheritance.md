# ciCommand can't express a multi-step local gate: single argv forces a hand-rolled `sh -lc`, and gaggle overrides silently deaden the local-ci task's own command

Suggested labels: area:workflows, area:contracts, documentation, type:bug

## Problem

A gaggle's local CI-equivalent gate is, for most non-Go stacks, more than one
command — install then test (Python, most npm builds), or build then test as
separate steps (Swift, and any stack that gates build and test failures
differently). `GaggleSpec.CICommand` can express only one. An author whose
real gate is two commands must either abandon the field and hand-write a
`sh -lc "cmd1 && cmd2"` invocation themselves (undocumented, un-reviewed,
introduces shell-quoting a pure-argv design was meant to avoid), or split
their gate into extra deterministic stages that duplicate the mechanism
`ciCommand` already exists to centralize.

A second, sharper problem sits directly underneath it: the deterministic
`local-ci` task in every shipped workflow already declares its own
`run.command` (typically `["make","ci"]`). When the owning gaggle sets
`ciCommand`, that declared command is silently and completely replaced —
the workflow file now contains dead YAML that nobody is told is dead. When
the gaggle does *not* set `ciCommand`, the task's own command is what
actually runs, live, and nothing tells the author that this is the
authoritative field in that case. Both states look identical on the page:
a `command:` list under `local-ci`. Only reading `gaggle.yaml` alongside the
workflow tells you which one is live, and `validate` checks neither
direction.

## Evidence

- `api/schemas/gaggle.schema.json:42-47` — `ciCommand` schema: `"type":
  "array"`, `"items": {"type": "string"}` — one argv, not a list of argvs.
- `internal/instance/gagglecapability.go:12-58` (`LocalCIStageName`,
  `ApplyGaggleCICommand`) — the actual override mechanism: when a gaggle
  declares `CICommand`, every workflow's `local-ci` task's `Run.Command` is
  unconditionally overwritten (`t.Run.Command = append([]string(nil),
  command...)`); when it does not, the task's own declared command is left
  untouched and is what the runner executes.
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-swift.md`
  ledger #2: `ciCommand`'s schema is a single argv; the assigned gate was
  `swift build` **and** `swift test`; only the stage literally named
  `local-ci` receives the override, so the second gate stage (`local-build`)
  is invisible to the gaggle-level declaration and the gaggle now
  under-describes its own gate. Quote: "A list-of-argvs, or a documented
  convention for multi-command gates, would remove the split entirely."
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-python.md`
  ledger #5: the shipped `python-service` reference's own `ciCommand:
  ["python3","-m","pytest","-q"]` fails on a normal src-layout package,
  which needs `pip install -e '.[dev]'` first — exactly what the repo's own
  CI does — forcing a hand-rolled `sh -lc` wrap because `ciCommand` is argv,
  not a shell line.
- Same file's "DSL ceremony notes": "the `local-ci` task carries a `command:`
  that the gaggle's `ciCommand` silently overrides — so the workflow file
  contains a stack-specific command that is dead when the gaggle sets one,
  and stale-but-live when it does not. I kept the two identical
  deliberately, but a smart default ... would remove a real footgun."
- Upstream origin: issue #1009 (MGV-1, closed) introduced `ciCommand`
  explicitly to solve "a Go gaggle runs `make ci`, an npm-built static-site
  gaggle runs `npm ci && npm run build`" — i.e., the multi-step case was the
  motivating example, but the shipped field ended up single-command.

## Proposed direction

1. **Widen `ciCommand` to accept a sequence of commands, additively.**
   Change the schema to `oneOf` a flat array of strings (today's shape,
   kept for every existing config) or an array of arrays of strings (a
   chain, each one a plain argv — no shell involved). `ApplyGaggleCICommand`
   expands a chain into a short run of deterministic stages
   (`local-ci`, `local-ci-2`, …) wired in sequence ending at the existing
   `local-ci` gate, so `LocalCIStageName`'s consumers (the runner,
   `openprbody.go`) need no change beyond reading the last stage's result.
   Each command still runs as a direct exec, not `sh -lc` — this preserves
   the argv-safety the field was designed around instead of trading it away
   for the shell string the coldstart testers had to invent unsupervised.
2. **Make the override direction visible.** Add a `validate` warning when a
   workflow's `local-ci` task declares a non-empty `run.command` *and* its
   gaggle also declares `ciCommand` — the task's command is guaranteed dead
   and the warning should say so, naming the field that actually wins
   (mirrors the existing pattern of `checkMissingSkillPackages` and other
   cross-file consistency checks already in `api/validate/validate.go`).
3. **Stopgap, ship immediately regardless of 1/2:** document today's
   supported multi-step pattern (`command: ["sh","-lc","cmd1 && cmd2"]`) in
   the `ciCommand` field's schema description and in
   `docs/guides/stack-support.md`'s bring-your-own row, so an author doesn't
   have to independently discover and hand-verify shell quoting the way both
   the python and swift cold-start runs did.

Zero-config behavior does not change: a gaggle with no `ciCommand` still
runs its `local-ci` task's own declared command untouched; a gaggle with a
single-command `ciCommand` (`["make","ci"]`, `["npm","run","ci"]`, …)
behaves exactly as it does today. The new chain form and the new warning
are both additive — no existing config's validated result changes.

## Alternatives considered

- **Leave `ciCommand` single-command and only document `sh -lc`.** Rejected
  as the whole fix: it permanently pushes shell-quoting risk onto every
  non-Go author (this is not a hypothetical — the coldstart python gate
  needed an editable-install prefix, a real and common shape) and keeps the
  field honest for Go/`.NET`/single-step Node gates only, contrary to the
  operator's stack-neutrality direction. Still worth shipping alone and
  immediately as the stopgap in item 3 above, since it costs nothing and
  helps today regardless of when items 1/2 land.
- **Give the `local-ci` task itself a `commands: [][]string` field,
  independent of the gaggle-level override.** Rejected: this reintroduces a
  second authority for the same concept MGV-1 was built to centralize —
  exactly the two-command ambiguity (task's own command vs. gaggle's
  override) that is already the second half of this problem.
- **Say nothing about the override direction, rely on authors reading both
  files.** Status quo; two independent cold-start flavors found this by
  hand, unprompted, in a single-repo exercise — a real production instance
  with dozens of gaggles has no comparable defense.

## Duplicate search

2026-08-08, `gh issue list -R Agent-Clubhouse/Goobers --search "<term>"
--state all`, terms: `ciCommand`, `local-ci multiple commands`, `ciCommand
shell`, `local-ci command override`, `task command overridden gaggle`,
`multi-step CI command`, `ciCommand list of commands`, `sh -lc ciCommand`,
`pip install editable ciCommand`.

Nearest hits, all non-covering:
- **#1009** (MGV-1, closed) — introduced `ciCommand` itself; explicitly
  scoped to "declare a CI command per gaggle" as a single override, never
  revisited for multi-step chains.
- **#2071** (closed) — `init` defaulting `ciCommand` to `make ci` with no
  stack detection; about the *default value* `init` writes, not the field's
  shape.
- **#2173** (closed, TSN-6) — guided init scaffolding a stale `make ci`
  regardless of the answered command; about `init`'s scaffolding bug, not
  the schema.
- **#2064** (open, tech-stack neutrality umbrella) — scoped to stack
  *language* support (Java/.NET/Python first-class); its own body
  explicitly separates that from CI *topology*, and its checklist does not
  mention chained commands or the task/gaggle override visibility gap.
- No issue anywhere mentions a list-of-argvs `ciCommand` shape, a
  `local-ci` task/gaggle override warning, or the specific src-layout
  editable-install / `swift build`+`swift test` failure modes.

## Size and risk

**M.** Schema change is additive (`oneOf`, existing single-array configs
unaffected) but touches `ApplyGaggleCICommand`'s stage-expansion logic and
anything keying on a workflow having exactly one `local-ci` task by name
(`internal/runner`, `cmd/goobers/openprbody.go` per
`gagglecapability.go`'s own doc comment) — those need to keep resolving to
the *last* expanded stage's result. The validate warning is low-risk and
independently shippable. The docs stopgap (item 3) is S and has no code
dependency; ship it first regardless of the rest.
