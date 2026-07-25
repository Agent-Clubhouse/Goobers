# Design: Separate GitHub repository sink for docs-updater

> **Status:** Accepted design contract (2026-07-25); runtime implementation is
> tracked by #1495.
>
> **Related:** #472 (docs-updater epic), #1016 (`docsRoots`), #1019 (this
> decision), #1020 (GitHub wiki), #1021 (Azure DevOps wiki), and #1464
> (`AdditionalRepos` credential routing).
>
> **Architecture:** [`ARCHITECTURE.md` §2 and §5](../ARCHITECTURE.md) and
> [`multi-gaggle-validation.md` §2](v1/multi-gaggle-validation.md).

## Decision

A docs-updater workflow may publish to either its gaggle's project repository
or one distinct GitHub repository. The workflow's gaggle project is always the
**source**. A `docsSink` selects the **target**; omitting it preserves today's
in-repo behavior.

A separate repository is a foreign sink even when it has the same GitHub
owner as the source. The source is read-only for this workflow. The sink gets a
separate, repository-qualified `github:pr:write` grant that covers its checkout,
run branch, and pull request. The source credential is never a fallback for the
sink.

The foreign-sink workflow only proposes a pull request. It cannot merge the
pull request or enable GitHub auto-merge. Review and merge belong to the target
repository.

## Configuration contract

The source remains the existing `Gaggle.spec.project`; it is not repeated on
the workflow, so two source declarations cannot drift. `Workflow.spec` gains:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: docs-updater
spec:
  gaggle: product
  docsRoots:
    - docs
    - README.md
  docsSink:
    kind: docs-repo
    repository:
      provider: github
      owner: acme-docs
      name: product-docs
      branch: main
      connectionRef: product-docs-pr
  # triggers, tasks, and gates omitted
```

`docsSink` has exactly two forms:

| Form | Meaning |
|---|---|
| omitted, or `kind: in-repo` with no `repository` | Read and write `Gaggle.spec.project`; current behavior. |
| `kind: docs-repo` with one `repository` | Read the gaggle project and propose changes in the named GitHub repository. |

`repository` uses the existing `RepoRef` shape. For `docs-repo`, `provider`,
`owner`, `name`, and `branch` are required. `provider` must be `github`.
`project` is forbidden because it is an Azure DevOps field. `connectionRef` is
required when the deployment uses Manifest connections and names the target's
write-capable connection.

The private local-runner configuration binds both identities independently:

```yaml
repos:
  - provider: github
    owner: acme
    name: product
    token: {env: PRODUCT_SOURCE_READ_TOKEN}
  - provider: github
    owner: acme-docs
    name: product-docs
    token: {env: PRODUCT_DOCS_PR_TOKEN}
```

There is no token, credential alias, source override, or `autoMerge` field in
`docsSink`. At local tiers, the exact repository identity selects one
`instance.yaml repos[]` binding. At connection-backed tiers, the exact identity
and `connectionRef` select one compatible GitHub connection. A global
unqualified credential override must not replace either repository-qualified
selection.

`docsRoots` keeps its existing ordered, repo-relative shape. For an in-repo
sink it is relative to the source project. For `docs-repo` it is relative to
the target repository and is the write boundary there. Target-root existence
is therefore checked against the target checkout, not the source checkout.

### Relationship to `AdditionalRepos`

`Gaggle.spec.additionalRepos` remains read-only by construction. A docs sink
must not be added there merely to acquire write access, and declaring a
`docsSink` does not silently change an existing `AdditionalRepos` entry into a
write target.

#1495 must reuse the routing shipped in #1464: the normalized repository
identity, `RepoBinding`, `RepoScopedCapability`, resolver, redaction
registration, and fail-closed grant lookup. The implementation should
generalize the existing repo-qualified grant builder so the sink is another
per-gaggle repository purpose, not create a second token list or credential
resolver. Read-only `AdditionalRepos` continue to receive only
`contents:read@owner/name`.

## Repository identity and validation

For this GitHub-only sink, repository identity is the case-insensitive tuple
`(provider, owner, name)`. Branch and credential references do not change
identity. Validation uses the following rules:

1. The workflow's gaggle and its `spec.project` must resolve before the sink is
   considered.
2. A `docs-repo` target must be a complete GitHub `RepoRef` with an explicit
   base branch.
3. Source and target identities must differ. A different branch or credential
   on the same repository is not a foreign sink.
4. The target must have exactly one deployment credential binding. Missing and
   duplicate matches are errors; list order is never a selection rule.
5. The source read binding and target write binding must use distinct
   credential references. Structurally identical env, file, store, GitHub App
   installation, or connection references are rejected.
6. When `connectionRef` is used, it must resolve, name a GitHub connection,
   match the repository's provider, and differ from the source connection.
7. Every `docsRoot` must pass the existing lexical containment checks. Its
   existence and resolved containment are checked in the target tree.

Same-owner and cross-owner sinks follow the same rules. Sharing an owner does
not permit credential reuse. Crossing owners does not require a special mode:
the exact target binding must independently authenticate to the target owner.
No owner substitution, first-repository default, ambient `GH_TOKEN`, or source
credential fallback is allowed.

GitHub wiki and Azure DevOps wiki identities are not accepted as `docs-repo`
targets. Their URL, branch, provider, and review semantics remain decisions for
#1020 and #1021.

## Two-grant capability flow

The credential route for a foreign sink contains two repository-qualified
grants:

```text
contents:read@acme/product
    -> PRODUCT_SOURCE_READ_TOKEN

github:pr:write@acme-docs/product-docs
    -> PRODUCT_DOCS_PR_TOKEN
```

The source grant permits clone/fetch and read-only analysis of the source
revision. It supplies no source `repo:push` or PR-write authority.

The target grant requires GitHub **Pull requests: read/write** and **Contents:
read/write**, matching the existing `github:pr:write` token contract. For the
built-in docs-repository sink transaction it authorizes target clone/fetch,
creation and push of the run branch, and opening or updating the target pull
request. It does not imply `github:pr:merge`, issue writes, access to another
repository, or an unqualified `repo:push` grant.

The runner qualifies the base capability with the repository selected by the
operation. A stage cannot request an arbitrary `@owner/name` string from
workflow YAML. The compiler admits the base capability, and the runner derives
the qualified key from the validated source or sink identity. This prevents
untrusted workflow inputs from redirecting a valid credential.

### Secret isolation

- Source checkout code can resolve only the source read grant. It never sees
  the target token.
- Target checkout and publish code can resolve only the target PR-write grant.
  It never sees the source token.
- The docs agent receives already-materialized workspaces and evidence. It
  receives neither repository token; `agent:model`, when declared, remains a
  separate grant.
- Git authentication is command-scoped. Credentials are not written into git
  remotes, credential stores, artifacts, invocation envelopes, or result
  envelopes.
- Both resolved values are registered with the journal scrubber before use.
  Logs and errors identify the capability and repository, never the ref's
  resolved value.

Distinct reference names alone are not enough at runtime. Preflight compares
non-reversible in-memory fingerprints of resolved secrets and fails if static
PATs resolve to the same value. GitHub App credentials must use distinct
repository-scoped installation bindings; the App private key is never exposed
to a stage.

## Fail-closed preflight

Foreign-sink admission completes before either repository is checked out and
before any branch, commit, push, pull request, comment, or label mutation:

1. Compile and cross-validate the source, sink, docs roots, exact deployment
   bindings, and distinct credential references.
2. Resolve both credential sources. A missing env var, unreadable file, failed
   store lookup, or failed App-token mint aborts admission.
3. Probe the exact source repository with the source credential and require
   read access.
4. Probe the exact target repository and configured base branch with the
   target credential and require repository write access sufficient for
   Contents and pull requests.
5. Reject an identity, owner, repository, branch, or permission mismatch. A
   provider timeout or indeterminate permission result also fails closed.
6. Pin the admitted source and target base SHAs in the run input, then begin
   checkout.

These probes are read-only provider operations. A failed preflight creates no
working copy and performs no external mutation. In particular, the runner must
not "try the source token" after a target probe fails.

## Checkout, change, and publish contract

After preflight, #1495 implements this fixed sequence:

1. Check out the pinned source revision read-only and gather the churn/current
   tree evidence used by docs-updater.
2. Check out the pinned target base into a separate managed working copy.
   Create `BranchNamespace + "docs-updater/" + runID` from the target base, not
   from the source revision.
3. Present source evidence as read-only context and make the target checkout
   the only writable repository workspace for the docs agent.
4. Apply and validate changes. Every changed path must remain inside at least
   one target-relative `docsRoot`; an empty diff or an escaping path follows
   the existing docs-boundary behavior.
5. Re-check the target base according to the normal stale-base policy, commit,
   push the target run branch, and open or update exactly one pull request in
   the configured target repository and base branch.
6. Record source identity/SHA, target identity/base SHA, head branch/SHA, and
   target pull-request URL as ordinary journal evidence and external refs.
   Record no credential material.

Idempotent retry lookup is scoped by target repository, base branch, and exact
run head branch. A pull request or branch with the same name in the source or a
different target is never reused.

## Review and merge ownership

Foreign-sink pull requests are human-controlled by default:

- The publishing workflow does not declare `github:pr:merge`, run `merge-pr`,
  request GitHub auto-merge, or route the PR into the source repository's
  merge-review workflow.
- Target branch protection, required checks, CODEOWNERS, reviewers, and
  maintainers remain authoritative.
- The target credential is the PR author only. It confers no review or merge
  authority, and source-repository maintainers gain no target authority.

A target repository may independently opt into automation by configuring its
own reviewed merge workflow or native bot, under its own credentials and
branch rules, to select these PRs. That target-owned automation is outside the
foreign-sink run. There is deliberately no source-side switch that can bypass
target review.

## Implementation boundary

#1495 owns the additive `docsSink` API/schema/deep-copy surface, validation,
repo-qualified write-grant routing, preflight probes, dual checkout, target
docs-root validation, target branch push, and target PR creation described
above. Existing in-repo workflows remain byte-for-byte compatible when
`docsSink` is omitted.

This decision does not provision a repository or credential, change
`AdditionalRepos` into writable repositories, add merge authority, or define
GitHub/Azure DevOps wiki behavior.
