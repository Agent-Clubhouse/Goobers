# Guide: Reviewed cross-repository coordination

`goobers coordinate` reconciles one approved work plan owned by a named gaggle.
Separate repository-owning gaggles implement its children through their ordinary
one-issue/one-PR workflows. The coordinator creates and links issues, controls
their eligibility, observes PRs and releases, and closes the parent only after
exact reviewed integration evidence. It cannot merge, push code, create a
release, or deploy.

**Delivery boundary:** this is a local, manually invoked operator command for
GitHub.com, not an autonomous planning agent, scheduled workflow stage, daemon,
or tier-3 service. It refuses workflow-stage and pod execution. Use one owning
instance on one host; concurrent invocations must share that instance root and
its `coordination-locks` directory. Independent hosts/instances are not supported
as concurrent coordinators for the same parent. No CLI invocation here installs
or replaces an executable.

This is distinct from repository-local `decomposition` / `publish-batch`.
`ChildPlan` there has no target-repository selector: its children and native
dependency relationships remain in the publisher's one repository.
`additionalRepos` remain read-only reference checkouts, not publication targets.
This distinction addresses [Agent-Clubhouse/Goobers#5172](https://github.com/Agent-Clubhouse/Goobers/issues/5172).

## Dormant provisioning and explicit authority

Add this optional block to the operator-owned `instance.yaml`. Leaving both
approval maps empty is a valid dormant configuration; loading it starts nothing.
Existing binaries without this feature reject the new field rather than silently
enabling it.

```yaml
coordination:
  gaggles:
    - name: coordinator
      parentRepo: {provider: github, owner: acme, name: planning}
      targets:
        - repository: {provider: github, owner: acme, name: core}
          approval: reviewed-plan
        - repository: {provider: github, owner: acme, name: consumer}
          approval: reviewed-plan
      approvedPlans: {}
      approvedEvidence: {}
```

The schema is `api/schemas/instance.schema.json`. All three repositories must
already appear exactly once in `instance.yaml`'s `repos`, with their own
credential references. Omit `baseUrl`: enterprise endpoints and even alternate
explicit spellings of GitHub's default endpoint are rejected in this version,
preventing two identity spellings from addressing the same publication target.
The named coordinator must be a loaded gaggle whose project is the parent
repository. Each target must have exactly one *separate* loaded gaggle with that
project identity. A read-only `additionalRepos` entry does not satisfy this rule.
The command loads the materialized local `config` tree, not a remote workflow
source; materialize and validate that tree through the existing config workflow
before activation.

Configure each repo owner's implementation workflow with
`spec.readiness.maxConcurrentRuns: 1`, its ordinary single-issue selector, and
`goobers:ready` / `goobers:approved` eligibility gates. Do not enable automatic
merge/deploy workflows as part of coordination. The command releases at most one
pending child per repository within a plan, but does not replace existing
scheduler/run/PR ownership or serialize unrelated plans' implementation runs.
Keep owner workflows' non-manual triggers disabled until separately approved.

The runner-only `coordination:write` capability is deliberately **not**
stage-declarable or configurable under `credentials`. Only this deterministic
operator command materializes it, under a repository-qualified key, using the
exact target repository's configured token and the existing credential injector
and secret scrubber. It ignores instance `credentials` and `daemonIdentity`
overrides. No cross-repo token is passed to an agent, subprocess, shell, JSON
artifact, or journal. No `runner.envPassthrough` credential exception is needed.
Token references still use the existing resolver, including explicitly pinned
credential-store references; there is no ambient-account fallback or account
switch in the command.

Use exact-repository fine-grained tokens with Issues read/write, Pull requests
read-only, and Contents read-only. These permissions support issue
publication/approval and PR/release observation, **not** merge or deployment.
Provider permission breadth is not reduced by a capability name: keep actual
token material appropriately scoped. See [GitHub token scopes](github-token-scopes.md).
Protect the operator's instance config and token sources from implementer OS
identities; a same-user unrestricted shell is not a security sandbox.

## Author, review, approve, then publish

The concrete two-repository plan is
[`examples/coordination/plan.json`](../../examples/coordination/plan.json).
`TestCoordinationExampleEndToEnd` executes that exact file with in-process fake
providers: core must merge **and publish v2.0.0** before consumer becomes
eligible, and both children require pinned integration evidence before parent
completion.

A maintainer authors this JSON explicitly. Planning assistance may propose text,
but it cannot approve publication. Review every title/body, summary, repository,
dependency, release tag, and integration command. Public issue bodies come only
from reviewed child content plus deterministic coordination metadata and links
to explicitly authorized dependency repositories. Parent issue text, private
run context, transcripts, and credentials are never copied to child issues.
The parent identity itself is not automatically published into public children.

```powershell
goobers coordinate --gaggle coordinator --plan .\plan.json --check .\instance
```

This offline command validates structure, target authorization, explicit approval
policy, and acyclicity. It prints `planDigest` and `publications`, containing each
initial issue's exact title/body and durable marker. It neither resolves
credentials nor creates runtime state; it does not require existing approvals.
Review that output and record its lowercase SHA-256 under
`coordination.gaggles[].approvedPlans.<plan-id>` in trusted `instance.yaml`.
Protect and review that configuration change separately from the work plan.
Parent approval labels never authorize a new target repository or changed plan.

Only after approval, invoke one reconciliation pass:

```powershell
goobers coordinate --gaggle coordinator --plan .\plan.json --result .\result.json .\instance
```

The command never starts owner workflows. Existing owner selectors may pick up
newly ready issues if an operator previously enabled their triggers. All flags
must precede the optional instance-root argument.

### Plan JSON contract

The strict decoder rejects unknown properties and trailing JSON documents.
`version` is `1`; `id` is a stable plan identifier; `gaggle` names its authority.
`parent` contains `{repository: {provider, owner, name}, id: "<issue-number>"}`.
`summary` is reviewed parent tracking text. `integrationCommand` is the exact
command whose real passing output a maintainer must later attest.

`children` contains 1-100 entries, each with:

| Field | Meaning |
|---|---|
| `repository` | Explicit `{provider: "github", owner, name}` identity authorized by a target entry. |
| `id` | Stable logical child ID, not a provider issue number. |
| `title`, `body` | Explicitly reviewed publication text. No copied parent context. |
| `kind` | `implementation` or `task`. |
| `dependsOn` | Optional array of `{repository, id}` references to other logical children. Bare issue numbers are never dependency keys. |
| `completion` | `merged`, `release`, or `closed`. `closed` is allowed only for a non-code `task`. |
| `releaseTag` | Required only for `release`, an exact tag rather than a version range or "latest". |

An implementation issue closing is not proof of implementation. A task closing
with GitHub's `completed` reason can satisfy `closed`; `not_planned` means
cancelled/failed. A closed, unmerged PR is abandoned/failed. A required release
must be published, its tag resolve to the approved exact commit, and that commit
must contain the PR's merged commit according to GitHub's compare endpoint.

## Observe PRs and approve integration evidence

PR ownership is a reviewed binding, not guessed from private discussion or a
closing keyword. After an owner opens its one PR, author an evidence JSON file:

| Field | Meaning |
|---|---|
| `planDigest` | Exact canonical digest returned by `--check`. |
| `children` | Reviewed bindings, one per observed logical child: `repository`, `id`, provider `issue`, provider `pr`, exact `headSha`; after human merge, add exact `mergeSha`; for release requirements, add `releaseSha`. |
| `integration` | Initially omit. After real integration passes, contains `command`, `passed: true`, `artifactSha256`, and `children` with the complete exact binding vector. |

Issue/PR numbers are positive decimal strings. Git commit pins are full lowercase
40-character SHAs. Artifact digests are lowercase SHA-256. Each PR belongs to one
child issue; the same numeric PR in a different repository is a different PR.
The command re-reads each bound PR from its target provider. A moved head or
changed merge/release SHA blocks progress instead of accepting stale evidence.
An unbound implementation child remains pending, or blocked if its issue closed.

Run `--check --evidence .\evidence.json` to compute `evidenceDigest`, review the
bindings against the live PRs, and record that digest under
`approvedEvidence.<plan-id>`. The plan digest is unchanged by evidence updates.
Use `--evidence` on subsequent passes; evidence is never implicitly trusted.

After all children satisfy their own and their dependencies' completion
conditions, the result is `integration-required`. Run the plan's real integration
command outside the coordinator against the exact landed/released revisions.
Save its actual output, inspect the exit code and results, and compute the log's
SHA-256 (for example with `Get-FileHash -Algorithm SHA256`). The maintainer then
attests the matching command, passing outcome, log digest, and **all** child
bindings under `integration`, reviews the new evidence digest, and approves it.
The coordinator does not execute the integration command or independently infer
whether arbitrary log text proves success; the outcome is an explicit reviewed
attestation plus exact artifact/pin verification, not a placeholder success.

```powershell
goobers coordinate --gaggle coordinator --plan .\plan.json --evidence .\evidence.json --check .\instance
# Review and approve the new evidenceDigest in trusted instance.yaml.
goobers coordinate --gaggle coordinator --plan .\plan.json --evidence .\evidence.json --artifact .\integration.log --result .\result.json .\instance
```

Only a matching real local artifact, exact approved command and passing
attestation, exact live pins, and a complete child vector permit `complete` and
parent closure. Rerunning without that evidence does not preserve a stale
success: incomplete or changed observations reopen a previously coordinated
parent. Provider errors are explicit and cannot be interpreted as completion.

## Result, retries, failures and recovery

JSON is printed to stdout and optionally written to `--result`. Its contract is
`planId`, `planDigest`, `state`, optional `reason`, and `children`. Each child
includes its qualified `repository` and logical `id`, actual `issue` / `pr`,
`state`, `reason`, separate `issueClosed` / `prMerged` booleans, `headSha`,
`mergeSha`, `releaseTag` and `releaseSha` when known.

Exit `0` means the pass or offline check ran successfully, **not** that the
parent completed. Inspect `state`: `waiting`, `blocked`, `integration-required`,
or `complete`. Child states include `ready`, `in-review`, `blocked`, `failed`,
and `complete`. Exit `1` means admission, provider, evidence, or persistence
failure; a live-pass result is `error` when reconciliation failed. Exit `2` is
usage. Never gate integration or downstream workflows on process exit alone.

The parent contains an immutable plan-digest pin, create-intent markers, and a
replaceable tracking section. Children contain exact durable body markers.
Publication uses the existing authoritative paginated marker listing, not
eventually indexed GitHub search. A shared local OS lock serializes all plans
for a repository-qualified parent; it releases on process death. Create intent
is persisted in the parent **before** each POST. A lost create response can
therefore be resumed by finding the original issue without creating another.
All children are published/linked/observed before any new eligibility release.
Issue edits use the provider's expected-revision guard.

| Condition | Operator recovery |
|---|---|
| Provider unavailable or permission refused | Fix the exact target's credential/access or retry when healthy. No alternative ambient token or success fallback is used. A failure cannot release new dependents. |
| Intent exists but no issue is found | Do not delete the intent and blindly retry. Inspect the provider for the original issue; restore its exact marker if edited. If the request never created an issue, a maintainer can manually create exactly the title/body in `--check`'s `publications`; rerun to adopt that marker. This intentionally trades automatic recovery for duplicate prevention after ambiguous writes. |
| Duplicate marker | Human inspection must identify the canonical issue and remove the marker from the unintended duplicate. The coordinator will not choose one arbitrarily or delete issues. |
| Child publication text changed | Restore the reviewed content or stop this parent and author a separately reviewed follow-up plan. Changed text is never automatically approved. |
| Parent plan digest changed | Active parent plans are immutable. Restore the original reviewed file/config approval; scope changes need a new parent and plan. Never strip the pin/intent markers to reset an active plan. |
| Cancelled issue or abandoned PR | Parent reports blocked/failed and does not complete. A maintainer must reopen/correct the work or author a new approved follow-up; do not represent cancellation as a merge. |
| Head/merge/release pin changed | Re-inspect live revisions, rerun integration where relevant, and approve new evidence. Old integration evidence does not transfer. |
| Wrong/missing integration log, failed attestation, incomplete vector | Remains `integration-required`; provide real, complete, separately reviewed evidence. |

Do not delete locks while commands are running, run independent coordinators
against the same parent, or allow unrelated workflows to rewrite coordination
metadata or bypass eligibility. Previously started implementation runs are not
cancelled by this command; stop/repair them through existing operator controls.
The command creates no persistent polling loop or automatic restart service.
