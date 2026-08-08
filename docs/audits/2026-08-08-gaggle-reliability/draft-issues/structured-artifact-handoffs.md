# Structured artifact handoffs: declare the shape of one item, validate every item at the stage boundary

Suggested labels: `area:contracts`, `area:runner`, `type:feature`, `goobers:needs-human`

## Problem

An artifact handed from a shaping stage to a filing stage has a filename and nothing else. The runtime checks that the declared file exists and that its path stays inside the workspace; it never looks at the bytes. Every constraint that actually matters — "this is a list", "each element is one work item", "each item carries a body, not a one-line stub", "evidence is present" — lives only in prose instructions, and holds only for as long as an agent reads forty lines of defensive text correctly on every single run.

The consequences a user experiences:

- A fan-out stage reports success, the join stage receives an inert filename or an unreadable absolute path, and the workflow completes green with zero output. Nothing in the run status distinguishes this from an honest empty result.
- A shaping stage emits a syntactically perfect document whose items are wrong — a whole plan pasted into one item, or fifty-five ~400-byte stubs where the workflow needed real bodies — and the filing stage dutifully files them. The damage is external and permanent (issues, PRs, comments) before any human sees it.
- Agent-authored metadata inside the artifact is never checked against run facts, so a fabricated value propagates into every downstream product built from it.
- The filing stage's author is forced to write defensive prose asking "is this one item, or did you hand me the whole container?" — a correctness check that costs tokens on every run and passes or fails nondeterministically.

`expectedOutputs` already establishes the precedent that a stage boundary can carry a declared, checked contract — but it covers only scalar output *key names*, and only at compile time. The structured payload, which is where the real handoffs live, has no equivalent.

## Evidence

- VISION wish 1, "Structured artifacts (needs upstream)" — `goobers-instances` `VISION.md` (branch `mason/dsl-2.1-wishlist`): a stage handing off a plan or findings list should declare the shape of one item and have the runtime validate each unit crossing the boundary, "the same way `expectedOutputs` already validates scalar outputs today." Marked *needs upstream*, i.e. the instance cannot close it in config.
- `docs/audits/2026-08-08-gaggle-reliability/domains/audit-nomination-flows.md`, finding `artifact-handoff-contract-retrofit` [HIGH/confirmed]: the `artifactFile`/`InputArtifactFile` handoff contract was the single root cause of 6 of quality-sprint's 7 bad runs. Observed blocked codes across those runs: `ARTIFACT_ACCESS_DENIED`, `FINDINGS_ARTIFACTS_UNAVAILABLE`, `MISSING_BACKLOG_PLAN`, plus 6-of-8 lens stages failing `missing_declared_artifact`. The finding's own conclusion after the contract was declared everywhere: "artifact placement depends entirely on agent compliance, and only declared artifacts are checked."
- Same file, finding `no-work-trusted-silent-loss` [HIGH/confirmed]: run `313af282743c533c51f75ec9cab0b038` — all 8 lens stages succeeded, every `findingsRef` pointed outside the workspace, `artifact_sizes:[]`, and the run recorded `completed` with zero filings. Total silent data loss behind a green run.
- Same file, finding `upstream-sync-value-was-rescued-not-produced` [MEDIUM/confirmed]: a 22,148-byte plan artifact yielded 55 filed issues at ~400 bytes of body each. The document was structurally legal; the items were not usable, and the value was reconstructed by a 2.5-hour out-of-band human sweep.
- Same file, finding `hallucinated-date-in-published-artifacts` [LOW/confirmed]: `"generated":"2026-09-15T14:30:00Z"` in the plan artifact propagated into all 55 issue footers, because "nothing validates agent-authored metadata against run facts."
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-dotnet.md` line 89: "The agentic-stage output convention is undocumented: quality-sprint pairs `inputs.artifactFile: findings.md` with `expectedOutputs: [findingsRef]` … but nothing explains where the name `findingsRef` comes from or whether an agentic stage can emit arbitrary scalar outputs — which is why I avoided a verdict-passing design." A fresh author declined to build a handoff because its contract was unreadable.
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/README.md`, systemic finding 2: "Prose knows what tooling doesn't enforce." Across five fresh-eyes onboardings, `validate` caught 32% of the friction (14 of 44 tweaks); the rest was found by guessing, by contradiction, or at runtime.
- Upstream code (worktree at `86ad1f70`):
  - `internal/harness/executor.go:70-78` — `InputArtifactFile` is one workspace-relative path; the guarantee is existence and journal capture, nothing about content.
  - `internal/harness/executor.go:318-320` — the complete artifact failure vocabulary today: `missing_declared_artifact`, `declared_artifact_path_escape`.
  - `internal/mcpio/tools.go:62-79` — `PublishOutput` resolves the path and calls `os.WriteFile`; the content is never inspected.
  - `internal/workflow/v_next/stagecontract.go:128-152, 211-219, 338-341` — `expectedOutputs` is enforced as a *validate-time* producer/consumer key contract. Grepping `ExpectedOutputs` under `internal/runner` and `internal/harness` returns no non-test hits: there is no runtime output-content check to extend, only a compile-time one to parallel.
  - `api/schemas/workflow.schema.json` — `task.inputs` is `additionalProperties: {type: string}` (so no structured contract can be expressed today), and `task.expectedOutputs` is still described as "Accepted for forward compatibility but not enforced by the local runner", which is stale relative to `stagecontract.go`.
  - `api/schemas/result.schema.json` — `outputs.additionalProperties` is scalar-only, which is why structured handoffs are pushed into artifacts in the first place.

## Proposed direction

Add an optional `artifactContract` to a task, in DSL 2.0 (`internal/workflow/v_next`) only.

```yaml
- name: shape-backlog
  goober: quality-triage
  inputs:
    artifactFile: backlog-plan.json
  artifactContract:
    items: "$.items"        # pointer to the list; default = document root when it is a JSON array
    minItems: 0             # default 0 — an empty list is legal until an author says otherwise
    item:                   # JSON Schema (2020-12 subset) for exactly ONE element
      type: object
      required: [title, body, evidence]
      properties:
        title:    {type: string, minLength: 8, maxLength: 120}
        body:     {type: string, minLength: 200}
        evidence: {type: array, minItems: 1}
```

Enforce it at three points, ordered by how early they catch the mistake:

1. **In-session, via the goobers-io MCP (the point that matters).** `publish_output` validates before it writes and returns the violations to the agent — item index, JSON pointer, failing constraint — so the agent corrects the artifact inside the session it is already in, at no extra session cost. Add a `describe_output_contract` tool so an agent reads the schema instead of inferring it from instruction prose. This builds directly on the already-merged `goobers-io` publish/read toolset, and is the reason to prefer a machine contract over better prose.
2. **At lift.** `liftArtifactFile` re-validates the bytes actually on disk, because an agent can bypass the tool and write the file directly. A violation fails the stage closed under a new typed code `artifact_contract_violation`, alongside today's `missing_declared_artifact`.
3. **At read.** `inputsFrom` materialization validates the artifact the consumer receives against the consumer's own declared expectation, so a stale, hand-edited, or cross-run artifact cannot enter a filing stage unchecked.

`goobers validate` compiles the `item:` schema and rejects an invalid one, and errors when `artifactContract` is declared on a stage with no `inputs.artifactFile` — the accepted-but-inert-field class `validate` already warns about.

Treat a contract violation as a **correctable** failure: retryable, with the agent given the violation text, rather than a terminal stage failure. Bound those retries with the single declared budget primitive proposed in the sibling draft (`one-budget-primitive.md`), not with a new hand-listed input — adding a budget input here would reproduce exactly the defect that draft exists to remove.

**Scope boundary.** A declared contract gives a `no-work` cross-check something concrete to check against, but this proposal does not change `no-work` semantics; that is drafted separately in this bundle (`no-work-verified-against-journaled-artifacts.md`). The two compose — a stage whose contract declares `minItems: 1` and which publishes nothing is no longer indistinguishable from an honest empty result — and should be reviewed together, but neither depends on the other landing first.

**Smart defaults — what zero-config does.** Nothing changes. A stage with no `artifactContract` behaves exactly as today: existence and containment only. No shipped workflow gains a failure mode, and there is no migration. The contract is the progressive-disclosure rung above `artifactFile`: the reference nomination workflows ship one so a first-hour author sees a worked example and copies it, `goobers explain` documents the field, and `goobers init` scaffolds it for the shaping→filing pattern. Deliberately **not** inferred or defaulted-on — a schema guessed from observed output would fail closed the first time an agent legitimately changed shape, which is the opposite of trustworthy.

`artifactContract` is a property of the boundary, not of the harness, so deterministic `run.script` / `run.command` producers get it on the same terms as agentic ones. That is what keeps it usable under the both-stages-and-scripts direction rather than being an agent-only feature.

**Position relative to the DSL 2.0 epic.** This is new surface and belongs in `v_next` only, which is consistent with that epic's measured finding that `v_next` is already a strict superset of `v_current` and with deprecating 1.4 — adding surface to one interpreter does not reopen the earlier policy-action parity drift. Sequence it after the epic's pin-only `goobers fix` fast path and its reference-workflow migration, so the reference workflows are already on 2.0 when they gain contracts.

## Alternatives considered

- **Generalize the completion-contract prompt hint.** Why not: four prior incidents each patched one field name in prose, and the next invented field name bypassed it (lineage in Evidence and Duplicate search). More decisively, the failures in the evidence above are documents that satisfy `result.schema.json` perfectly; no prompt hint can catch a well-formed plan whose items are stubs.
- **Relax `outputs` to admit structured values.** Why not: outputs are the run's routing keys. `inputsFrom` threads bare scalars (`internal/runner/inputsfrom.go`), gates and policy actions read them, and they are indexed in the journal. The open prompt-hint issue's own recommended fix text directs structured data into artifacts; this proposal puts the checking where that structure already lives.
- **A per-workflow validator stage (`run.script` + `jq`).** Why not: it works today and should stay available for domain rules a schema cannot express, but it runs after the agent session has ended (no in-session correction), it is per-workflow copy-paste, and hand-copied stages are precisely what drifts between gaggles.
- **One whole-document JSON Schema instead of a per-item one.** Why not: equal expressive power, worse ergonomics and worse errors. The author re-declares the container each time, and a violation reports `/items/17/title` inside a 22KB document instead of naming the offending item. Per-item keeps the declaration to one screen and the error actionable by the agent that has to fix it.
- **Extend the open DSL-declared-MCP-tool-schemas proposal to cover this.** Why not: that proposal types the *tools* a stage may call. This types the *data* crossing a boundary, which must hold for deterministic producers that never open an MCP session.

## Duplicate search

Searched 2026-08-08 (read-only), `gh search issues --repo Agent-Clubhouse/Goobers`, open and closed, plus `gh issue view` on each candidate. Terms: `structured artifact`, `artifact schema`, `output schema`, `typed output`, `list-shaped`, `per-item validation`, `per-item`, `artifact contract`, `handoff schema`, `handoff contract`, `typed handoff`, `item schema`, `schema for one item`, `declared shape`, `typed artifact`, `resultShapeHint`, `expectedOutputs`, `artifactFile`, `publish_output`, `validate artifact`, `schema validation stage`.

Nearest existing issues and the delta:

- **#2522** (open, 2026-08-07) — *resultShapeHint only patches scalar-only outputs rule per-field*. Adjacent, not covering: it fixes the prompt hint for scalar `outputs`, an envelope-schema concern. It explicitly directs structured data into artifacts and proposes no checking there.
- **#1484** (open, 2026-07-25) — *Evidence-artifact schema + durable emission for investigations*. Scoped to persisting repro harnesses/dumps for the deep-investigation workflow (split from #816); no cross-stage handoff contract, no per-item validation, no DSL surface.
- **#2407** (open, 2026-08-04) — *DSL-declared MCP tool schemas*. Types tool interfaces for agentic stages; does not type artifacts and does not apply to deterministic producers.
- **#2422** (open, 2026-08-05) — *Publish workflow artifacts atomically and bind completion to the published generation*. Durability and torn-write integrity of `publish_output`; explicitly about which bytes land, not whether those bytes are correct. Complementary — both touch `internal/mcpio/tools.go` and `liftArtifactFile`, and the digest/generation token #2422 introduces is a natural place to record "validated against contract vN".
- **#2406** (closed/merged, 2026-08-04) and **#2414** (closed) — built the `goobers-io` publish/read tools and wired them into shipped workflows. They are the mechanism this proposal validates through; neither validates content.
- **#881 / #565 / #907 / #900 / #905** — the `expectedOutputs` lineage: established it was inert, then made it a validate-time contract with a warning surface. This proposal is the structured sibling of that contract; the lineage covers scalar key names only.
- **#299 / #302 / #297 / #304 / #301** (all closed) — the incident class this prevents: instructions asking for shapes the runtime then rejected or silently accepted. Each was fixed by editing instructions or one prompt line; none introduced a checkable contract.
- **#2695** (open epic, 2026-08-08) and children #2696–#2700 — DSL 2.0 / 1.4 deprecation. Purely a version-lifecycle epic; adds no stage-contract surface. This proposal targets 2.0 and sequences behind #2696/#2698.
- **#1310** (open) — parallel failure policy and bounded `for_each`; control-flow surface, not data contracts.

No open or closed issue proposes declaring the shape of a handoff artifact or validating items at the boundary. Filing this whole, not narrowed.

## Size and risk

**Size: M.**

Blast radius (additive, opt-in):
- `api/schemas/workflow.schema.json` — new optional `task.artifactContract`.
- `internal/workflow/v_next` — model, feature ID, compile, and validate-time schema compilation (`v_current` untouched, per the DSL 2.0 direction).
- `internal/harness/executor.go` — validation in `liftArtifactFile`, one new typed error code.
- `internal/mcpio` — validation in `PublishOutput`, plus a `describe_output_contract` tool and its toolset config.
- `internal/runner` — validation at `inputsFrom` materialization.
- Docs: `docs/stage-contract.md`, feature matrix, `goobers explain`, and the reference nomination workflows as worked examples.

Migration: none. Absent `artifactContract`, behavior is byte-identical to today; no existing workflow, gaggle, or instance config changes. The reference-workflow examples are the only shipped YAML that gains the field.

Risks and mitigations:
- *An over-strict schema turns useful agent output into a hard failure.* Mitigated by in-session validation with actionable messages (the agent fixes it before the session ends), by correctable-retry semantics rather than terminal failure, and by `minItems: 0` defaulting so "found nothing" stays legal.
- *Validation cost on large artifacts.* Per-item validation over a 22KB plan is negligible; cap document size and item count in the validator so a pathological artifact fails fast rather than stalling a stage.
- *A second schema dialect for authors to learn.* Mitigated by using the JSON Schema subset already vendored (`github.com/santhosh-tekuri/jsonschema/v5`) and already used for `result.schema.json`, and by shipping worked examples rather than a reference grammar.
- *Interaction with the open atomic-publish proposal.* If that lands first, validate before the atomic replace and record the contract version alongside its generation token. If this lands first, that change wraps an already-validated write. Either order works; both touch the same two functions, which is worth noting to whoever schedules them.
