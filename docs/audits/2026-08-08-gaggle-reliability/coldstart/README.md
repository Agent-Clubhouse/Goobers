# Cold-start onboarding exercise — 2026-08-08

Five fresh-eyes agents, each onboarding one testbed repo using **only shipped
materials** (binary + `--help`, docs/, examples/, config-examples/) — the
production instance, the audit, and the Go source were explicitly forbidden.
Each implemented a different loop pattern and kept a strict friction ledger.
Binary under test: `04d0d963-dirty` (main + this branch's Phase C fixes).
Scope: authoring to a clean `goobers validate` — no daemon runs. Per-flavor
ledgers with verbatim errors are in this directory.

## Scorecard

| flavor | pattern | done | tweaks | caught by validate | found other ways | YAML authored |
|---|---|---|---|---|---|---|
| minimal | degenerate single-agent → merge | ✅ | 5 | **4** | 1 | 30 |
| python | standard curation + implementation | ✅ | 10 | **1** | 9 | 186 |
| dotnet | fan-out dual-review | ✅ | 12 | 4 | 8 | 185 |
| swift | local-gate-only, zero hosted CI | ✅ | 10 | 2 | 8 | 275 |
| ado | curation + implementation on ADO | ✅ | 7 | 3 | 4 | 557 |
| **total** | | 5/5 | **44** | **14 (32%)** | **30 (68%)** | |

Classification: docs-gap 15 · default-assumption 14 · dsl-ceremony 9 ·
capability-expectation 3 · invariant-assumption 2 · honest user error 1.
Discovery: validate 14 · guessing 11 · docs-contradiction 9 · cli-help 6 ·
later-command-error 4.

**Headline: every flavor reached a validate-clean config, and only 32% of the
friction was caught up front.** The scaffolded/guided path (minimal) had the
best guardrails (4/5); the flagship standard pair had the worst (1/10) —
"validate-clean" is a much weaker signal than a first-hour user assumes.

## The big five systemic findings

1. **Silent no-work is the #1 first-hour killer.** Three flavors independently
   hit "my backlog selector matches nothing": the scaffold's default
   `labels: [goobers]` / `trustLabel` matches zero labels on a real repo, and
   nothing — not `connect`, not `--check-repos`, which both *contact the
   repo* — compares selectors against the target's actual labels/tags or
   eligible-item count. The loop runs forever claiming nothing,
   indistinguishable from an idle daemon. Probes extended the class: a full
   ci-poll workflow against the zero-CI repo validates clean (every run would
   park at the gate timeout); `requiredCapabilities: [nosuchtoolchain@42]`
   with an empty `runner:` block validates clean (every run refused at
   schedule time); a gate `fail:` branch with `continueOnError` omitted is
   unreachable, silently (two of three shipped references omit it).
2. **Prose knows what tooling doesn't enforce.** The onboarding guide warns
   in prose about the default-label trap, the label bootstrap, and too-hot
   budgets — validate is silent on all of them. The recurring shape: the trap
   is *documented* and *unchecked*.
3. **The scaffold ladder has specific missing rungs.** `init` emits warnings
   on its own output (SKILL002 — all five flavors); both scaffolders emit
   `skills:` entries they don't create packages for; there is no
   `scaffold gaggle` and no rename path (5 fields, 2 files, 1 directory by
   hand); no non-interactive route to the standard pair (guided is
   prompt-only); `connect` rewrites repo coordinates but not gaggle identity
   or example prose — three instruction files still told a Python-repo agent
   it worked on "the Acme Web gaggle" and to avoid `npm run ci`.
4. **ADO is a first-class provider wrapped in a GitHub on-ramp.** The
   plumbing is genuinely provider-neutral (three-part identity, real
   authenticated reachability checks, did-you-mean cross-checks, an excellent
   auth guide). But `connect` rejects the ADO identity and — worse —
   *accepts* the two-part guess and writes a broken `provider: github` hybrid
   to disk. The five `ado:*` capability enum values are dead — no stage
   accepts them; every backlog stage on Azure Boards must declare the literal
   `github:issues:write`, and `report-pr-status` (the most ADO-native stage,
   invisible in all docs) rejects `ado:pr:status` with an error naming GitHub
   and Gitea. Zero ADO examples exist anywhere in the tree.
5. **Fan-out review is expressible, undiscoverable, and ceremonial.**
   `spec.parallels` requires `dslVersion: "2.0"` while `schema`, `init`, and
   every shipped example say 1.4 — discoverable only via a generated feature
   table and an implementer design doc. Expressing "two reviewers, both must
   pass" cost ~55 scaffolding lines of 90 (exit-1 stages to convert verdicts
   into branch failures, per-branch clones of shared park stages, dead
   `@join` edges for a reachability check) because a gate verdict cannot
   soft-fail its own branch (no `@fail-branch`), branches must be disjoint,
   and the default workspace is illegal inside any actually-parallel
   parallel (`maxConcurrentBranches` defaults to 1 — sequential). The text
   `workflow show` omits the parallel entirely; `--dot` shows it.

## Cross-cutting smaller findings

- `instance.yaml` — the first file `init` tells you to edit — has no schema
  kind, no `explain` surface, and its only real reference is a comment block
  in `reference-workflows/instance.yaml.example` (all five flavors).
- Toolchain reality is unchecked: under `sh -lc` the daemon's login-shell
  PATH resolved `python3` to 3.9.6 against a `requires-python >=3.12`
  package while validate stayed clean; the env allowlist covers
  Go/.NET/Python/Node/Rust but not Swift/Xcode, and nothing says so per
  stack; `ciCommand` is a single argv, so "install, then test" (most non-Go
  stacks, including the repos' own CI) needs a hand-rolled `sh -lc`.
- Capability + policy-action double declaration: 11 capability declarations
  and 14 policy-action entries across two workflows, all mechanically
  derivable from the built-in command — the validator knows the mapping (its
  errors name the exact missing string) but makes the author guess first.
- `ci-poll` is unauthorable from shipped docs: not in `help stages`, no man
  page, its contract lives in comments inside one example file, plus a
  schema-required `run.command` placeholder that never executes.
- Label-vocabulary contracts between stages are unchecked and undocumented
  (`goobers/status:in-review` vs `goobers:...` — silent forever-reclaim on
  mismatch).
- `connect --seed` under-seeds: derives selector labels only, not the labels
  the workflows *apply* (claim mirror, park/close-out statuses) — first run
  dies at first park. No ADO equivalent of `--seed` at all.

## What already works (preserve these)

`init` self-validating with a correct "Next:" list; `connect`'s
placeholder rewrite + token-by-name-only + live reachability (on GitHub);
`examples show` emitting near-production YAML with why-comments (the single
biggest success factor); `workflow show` DAG rendering; `explain --human`
printing complete enums; `validate --check-harness/--check-repos` doing real
liveness; stable diagnostic codes with paths; stack-support.md's honest tier
table; the ado-authentication guide. The onboarding *skeleton* is excellent —
the gaps are specific, and most are checks the tooling already has the
information to make.
