# The first-value ladder — five flavors, real daemon, e2e (2026-08-09)

Operator's bar: "success here is really the e2e daemon running on all these
disparate things else im not sure if we truly checked for real or made it
up." Method: the five testbed gaggles were adopted into the LIVE production
instance via `goobers apply` (itself under test), manual-trigger only,
laddered one flavor at a time, each proven end-to-end before the next.
Daemon: `7a89ad48` lineage. All instance-config fixes are commits on
`special-agent/config-2.0-reconciled`; every wall below is replayable from
that history.

## Results

| rung | flavor | attempts | proof |
|---|---|---|---|
| 1 | minimal — degenerate single-agent, no gates | 3 | issue claimed → implemented → PR #9 opened → **PR #8 merged autonomously** |
| 2 | python — standard curation+implementation pair | 2+2 | curation full arc (incl. reconcile/feedback/sample stages) → implement → review pass → local 3.12 pytest → PR #8 → hosted CI → issue closed in-review |
| 3 | swift — zero hosted CI, local-only gates | **1** | `swift build` + `swift test` (28 tests) gates → PR #7 → close-out; the no-checks-reported case as a working design |
| 4 | dotnet — parallel dual-review (`spec.parallels`, first live gates-in-branches) | 5 | push-first → both reviewers real verdicts on real diffs → all_or_nothing join → `dotnet test` (global.json→8.0.404) → PR #7 → hosted CI |
| 5 | ADO — provider crossing | 2 (impl) | WIQL claim → implement → local pytest → PAT push → **ADO PR 359** → native `report-pr-status` → pipeline `ci-poll` → **work-item closed** |

## Walls found (each fixed or filed; none reachable by any static layer)

**Product defects (fixed on `special-agent/reliability-hardening`):**
- Entitlement-gated rules API (free-plan private repo 403) classed as
  `github_auth_failed`, killing every merge — fixed: degrade to
  direct-merge, GitHub stays the enforcer (`7a89ad48` + regression test).

**Product gaps (draft-issue evidence):**
- `goobers apply` reloads workflow definitions but NOT instance.yaml
  credential/repo bindings — new repos 404 until restart, silently.
- Daemon readiness is O(all-runs-ever) journal reads (~5.5 min at 55K run
  dirs; stack-sampled), breaking `goobers run`'s 30s delegate window.
- `apply` returns before dispatch snapshots swap — a trigger fired
  immediately after can execute the previous definitions (settle needed).
- Parallel branch workspaces materialize from PUSHED state: branch gates
  reviewing an unpushed run branch see an empty diff structurally. (The
  runner's empty-diff refusal was honest; the implementer's commits sat
  correct in the mirror — push-before-fan-out is the config-level shape.)
- Toolchain probers answer from the wrong resolution context: pyenv shim +
  `sh -lc` (python resolves 3 different ways) and `global.json` per-directory
  SDK selection (dotnet) — claims the host satisfies get refused; claims the
  host can't satisfy would pass. Probe in the workspace or don't verify.
- ADO parity, failing closed with named tickets (the matrix discipline
  working): curation-mode claim blocked entirely (BL-033 — the guard is
  `curationRun`, so the `reconcileMetadata` input cannot bypass it);
  ready-label transition reads (readyAt trust annotation) reach parity in
  V1 — claiming on the trust label alone is the supported ADO shape today.
- Validate approves provider×stage combinations the capability matrix
  already knows are unsupported (the ADO curation config was
  validate-clean and can never run).

**Contract-communication class (config fixes; feeds structured-artifacts +
in-session validation drafts):** literal-validated contracts must be stated
literally in instructions — output key spelling (`changed-files`: two
sessions burned guessing camelCase/snake_case), verdict decision enum
(`pass|needs-changes|fail`), metrics values (numbers only). Completion
errors surface only after the session ends, so each guess costs a full
attempt.

**Operational ergonomics:** `maxRunsPerHour: 1` / `maxRunsPerDay: 2`
defaults turn any failed manual attempt into an hours-long retrigger
lockout; rename-fragility (a workflow rename silently broke a hardcoded
`headPrefixes`, yielding a completed-but-unmerged false green).

## What held up

Honest failure everywhere: typed error codes named each wall in one read;
journals held replayable evidence; the empty-diff guard refused
plausible-but-uncommitted success; unsupported provider ops named their gap
tickets; `apply` rejected an invalid config and kept last-known-good; the
per-repo PR cap held implementation closed while the site drained. Mean
time-to-diagnosis per wall: minutes. The system is easy to debug because it
tells the truth — which is the beta property that matters most.
