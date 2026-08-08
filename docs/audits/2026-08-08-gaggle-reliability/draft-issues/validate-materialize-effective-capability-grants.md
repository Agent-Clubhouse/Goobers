# `goobers validate` cannot answer "which token backs this gaggle's capability, and can it reach that gaggle's repo" — instance-level credential overrides silently misroute

Suggested labels: `area:runner`, `area:cli`, `type:bug`, `sprint:v1-breadth`

## Problem

In a multi-gaggle instance, an `instance.yaml` `credentials:` entry is applied to **every** gaggle. There is no way to say "this capability override applies to gaggle A only." The override replaces the repo-matched default grant for all gaggles, including gaggles whose target repo the override token cannot reach.

The operator gets no signal at author time. `goobers validate --check-repos` reports every repository reachable and exits 0, because it preflights `repos[]` tokens against their own repos and never materializes the grants a run will actually receive. The failure surfaces later as a production 403/404 inside a stage — an authorization error attributed to the stage, not to the config line that caused it.

Two separate defects compound:

1. **The config surface cannot express the intent.** A `credentials:` entry has no gaggle or repo qualifier, so an operator who needs a scoped override has only two choices: apply it instance-wide and break the other gaggle, or don't use it at all.
2. **Validation cannot see the consequence.** Nothing enumerates gaggle × capability → token, and nothing checks that the resolved token can reach that gaggle's target repo. A config that can never work certifies clean.

This is the "a capability grant that resolves to a token with no access to the target repo should be a validation error, not a production 403" property in the operator's charter, and it is currently unmet in both directions.

## Evidence

- `internal/credentials/scoping.go:55-60` — `RunnerGrants` applies every override unqualified: `grantRef[o.Capability] = o.Ref` replaces the repo-matched default for whichever gaggle is being built. The doc comment at line 26 states the design intent explicitly: "These stay unqualified (shared)."
- `internal/instance/config.go:929-939` — `CredentialGrant` is `{Capability, MCP, Token}`. No gaggle field, no repo field. The DSL cannot express a scoped override.
- `cmd/goobers/runnerwiring.go:546-555` (`cfg.Credentials` → overrides), `:334-336` (the credentialed-capability list) — blast radius is all 10 credentialed capabilities (`repo:push`, `pr:write`, `pr:review`, `pr:merge`, `branch:delete`, `issues:*`, `milestones:write`) × every gaggle.
- `cmd/goobers/validate.go:825-864` — `checkTargetRepositoriesAtFile` iterates `repos[]` only; `:871-893` — `resolveRepoToken` resolves from `repo.Token`/`repo.GitHubApp` and never consults `credentials:` refs. No credential-aware check exists anywhere in the validate path (`cmd/goobers/configwarnings.go` has zero `credential` hits).
- Audit: `docs/audits/2026-08-08-gaggle-reliability/domains/audit-code-audit.md` — findings `credentials-scoping` (HIGH/confirmed) and `credentials-validate-blindness` (MEDIUM/confirmed). The same file's `healthy-per-gaggle-scoping` finding confirms the per-run repo-matched path is otherwise sound (`internal/credentials/scoping.go:31-67`), which is what makes the unqualified override the sole misroute vector.
- Audit: `docs/audits/2026-08-08-gaggle-reliability/domains/audit-issue-inventory.md` — finding `gap-k4-credential-override-validation`: the validate-time ambiguity check has no upstream issue; only the downstream symptom is filed.
- Live reproduction (two-gaggle instance, 2026-08-08): a `github:pr:review` override intended for one gaggle routed that gaggle's token into the *other* gaggle's `apply-verdict`, producing a 404 on `POST /repos/{o}/{r}/pulls/{n}/reviews`. Both `validate` and `validate --check-repos` were clean before and after.
- Cold-start corroboration that inert credential/capability declarations validate clean: `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-ado.md` — `ado:work-items:write` "is accepted by the schema but is required by nothing … it validates clean under `--strict` as a dead, inert grant."

## Proposed direction

Ship the check first, the schema second. They are independently useful and the check is what stops the bleeding.

**1. `goobers validate` materializes the effective grant table (no new config required).**

For every gaggle × credentialed capability, resolve the grant exactly as `RunnerGrants` will at runtime and emit it as inspectable output:

```
GRANTS gaggle=goobers-site
  repo:push            <- credentials/site-token   (masra91/Goobers-Site)  reachable
  github:pr:review     <- credentials/dev5-token   (masra91/Goobers-Site)  UNREACHABLE (404)
  agent:model          <- credentials/model-token  (n/a — not repo-scoped)
```

Static tier (always, no network): a new error `CRED001` when an unqualified instance-level override lands on a gaggle whose target repo differs from the repo the override's token is bound to *and* the instance has more than one distinct target repo. In a single-repo instance nothing changes — the override is unambiguous there by construction, which is the case every existing config is in.

Network tier (`--check-repos` only, reusing the repository preflight that already contacted the repo): `CRED002`, one authenticated reachability probe per distinct (token, gaggle-repo) pair actually produced by the grant table — not per `repos[]` entry. Report as a repository observation with the same contract as the existing oversized-repo warning: printed, in the JSON diagnostics envelope, never changing exit code, because repo state and token permissions can change a minute later.

**2. Give `credentials:` an optional qualifier so the intent is expressible.**

Add optional `gaggle:` and `repos:` qualifiers to a credential entry. An unqualified entry keeps today's meaning — shared across every gaggle — so `agent:model`, the capability this surface was built for, needs no edit anywhere and the zero-config path is byte-identical. A qualified entry applies only where it matches, and `RunnerGrants` picks the most specific match (gaggle-qualified > repo-qualified > unqualified > repo-matched default). With a qualifier available, `CRED001` becomes actionable rather than a dead end: the diagnostic names the qualifier to add.

**Smart defaults / zero-config behavior:** an operator who writes nothing sees no new fields and no new errors. A single-gaggle or single-repo instance is untouched — `CRED001` cannot fire there. The grant table prints only under `validate --verbose` or `--json`; the default validate run stays quiet unless something is actually wrong. Progressive disclosure: the qualifier exists for the operator who has already hit the ambiguity, and the error message is where they discover it.

**3. Reconcile with the in-flight credential-model work rather than forking it.**

The MGV G6 arc (design PR #1793) already proposes a first-class `credentials:` list whose entries bind to one or more repos. That work is scoped to the `additionalRepos` read-credential path and its acceptance criteria say nothing about capability overrides, gaggle qualification, or grant materialization. The qualifier proposed here should land *as part of* that schema rather than beside it — one `credentials:` surface, not two — and that arc's schema issue should be corrected to cover the override case it currently omits. Item 1 (the check) does not depend on any of it and should not wait.

Position against the DSL 2.0 epic: this is instance-config surface, not workflow DSL, so it is not gated on the 1.4→2.0 migration. The qualifier is additive and unqualified-stays-shared, so it needs no DSL version bump.

## Alternatives considered

- **Make every override repo-qualified, mandatory.** Breaks every existing instance using a shared `agent:model` token — the exact case the surface was introduced for — for no safety gain, since a model token has no repo to be scoped to.
- **Refuse instance-level overrides entirely in multi-gaggle instances.** Removes a working capability (shared model tokens) to fix a routing bug, and leaves the operator with no way to express a per-gaggle override at all.
- **Runtime-only fix: pick the repo-matched grant when the override token can't reach the repo.** Silent recovery from a config the operator got wrong, deciding at 403-time which token they *meant*. Fails "config as truth" and hides the error where it is hardest to see.
- **Ship only the network check (`--check-repos`).** Leaves the ambiguity undetected in CI runs and pre-commit hooks that validate without network. The static tier is the one that catches the mistake early.
- **A separate `goobers credentials explain` command instead of validate output.** Discoverable only if you already suspect a problem. The check belongs where operators already look before they start the daemon.

## Duplicate search

Searched 2026-08-08 against `Agent-Clubhouse/Goobers` (open and closed), read-only:

Terms: `credential override`, `credential scoping`, `instance-wide credential`, `capability grant validate`, `validate token repo access`, `effective grants`, `credential qualifier gaggle`, `credential in:title`, `capability in:title validate`, `per-token in:title`, `MGV in:title`, `token identity in:title`.

Nearest existing issues and the delta:

- **#1794 MGV-13 (open) — "credential schema — first-class credentials list, repo(s)-bound not repo-owned."** Closest match, and it **shrinks this draft's part 2**. It already proposes a top-level `credentials:` list with `repos: [...]` binding. It does **not** cover: the *capability-override* path through `RunnerGrants` (its problem statement is `RepoRef`'s 1:1 credential and the `additionalRepos` read path), gaggle-level qualification, or any validation that materializes effective grants. Its acceptance criteria are shape validation and backward compatibility only. Delta: fold the gaggle/repo qualifier for capability overrides into #1794's schema, and correct #1794's scope to acknowledge that an instance-level `credentials:` list already exists today and is applied unqualified.
- **#1798 MGV-17 (open) — validate/lint migration diagnostic.** Warn-only, and specifically for `additionalRepos` entries missing an explicit `credential:`. Delta: says nothing about capability overrides or about whether a resolved token can reach a gaggle's repo.
- **#1795 MGV-14 / #1796 MGV-15 / #1799 MGV-18 (open).** All `additionalRepos` read-credential path: required `credential:` field, `AdditionalReadGrants` consuming it, isolation-conformance tests. The audit confirms `AdditionalReadGrants` is already sound (read-only, repo-qualified); the misroute is in the override path these do not touch.
- **#1178 / #1175 (closed) — "Validate provider-chain capability sufficiency at compile time."** Validates that a stage *declared* the capabilities its built-in needs. Delta: says nothing about which credential backs a declared capability or whether that credential can reach the repo. This is the check one layer down.
- **#1012 MGV-5 (closed), #1285 MGV-10 (closed).** Delivered the per-gaggle repo-matched routing that the audit confirms works. The override path was left unqualified by design in that same code.
- **#2691 (closed 2026-08-08) — `IsSelfReviewError` misses the fine-grained-PAT 404 signature.** The downstream symptom of exactly this misroute, fixed at the error-classification layer. Delta: makes the misroute degrade more gracefully; does nothing to detect it at author time.
- **#682 (open) — per-stage credential grants; #680 (open) — per-sandbox credential grants.** Both explicitly V2 backlog, not approved for automated implementation, and both add *finer* grant scopes below the gaggle. Delta: neither fixes the gaggle-level ambiguity that exists today, and #682's stated precedence chain (stage > goober > instance) assumes an instance grant that is unambiguous — which is the assumption this issue shows to be false.

Upstream files issues quickly; re-run this search at filing time.

## Size and risk

**Part 1 (validate grant materialization + `CRED001`/`CRED002`): M.** Blast radius: `cmd/goobers/validate.go` and a new grant-materialization call into `internal/credentials`. Risk is false positives failing a previously-clean config in CI — mitigated by scoping `CRED001` to multi-distinct-repo instances only (single-repo instances cannot trip it) and by making the network tier exit-code-neutral. Requires reusing `RunnerGrants` itself rather than reimplementing its precedence, or the check drifts from the runtime it is meant to model.

**Part 2 (credential qualifier): M, high blast radius.** Touches the credential-resolution core every gaggle runs on — same posture as MGV-5/MGV-13, and should carry the same explicit-review requirement. Migration: none required; unqualified entries retain today's semantics permanently, so no existing config changes. If it lands inside #1794's schema the marginal cost is one field and one precedence rule.

**Sequencing:** part 1 has no dependencies and should ship first. Part 2 should be merged into #1794's scope rather than filed as a competing schema change.
