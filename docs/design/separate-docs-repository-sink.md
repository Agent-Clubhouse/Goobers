# Design: Separate GitHub repository sink for docs-updater

> **Status:** approved — accepted design contract (2026-07-25); runtime implementation is
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
    writeRoots:
      - content/product
      - README.md
  # triggers, tasks, and gates omitted
```

This contract defines the two repository-backed `docsSink` forms below. The
union also reserves `kind: github-wiki` with the distinct direct-publication
contract in [`github-wiki-docs-sink.md`](github-wiki-docs-sink.md):

| Form | Meaning |
|---|---|
| omitted, or `kind: in-repo` with no `repository` or `writeRoots` | Read and write `Gaggle.spec.project`; `docsRoots` keeps its current dual role. |
| `kind: docs-repo` with one `repository` and non-empty `writeRoots` | Read and classify the gaggle project using `docsRoots`; propose changes in the named GitHub repository only within `writeRoots`. |

`repository` uses the existing `RepoRef` shape. For `docs-repo`, `provider`,
`owner`, `name`, and `branch` are required. `provider` must be `github`.
`project` is forbidden because it is an Azure DevOps field. `connectionRef` is
required when the deployment uses Manifest connections and names the target's
GitHub App installation connection. A foreign sink's target credential must be
introspectable `github-app` authentication; static PAT target bindings are
rejected by this contract for the preflight reason below.

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
    auth:
      kind: github-app
      appId: 123456
      installationId: 987654
      privateKey: {env: PRODUCT_DOCS_APP_KEY}
```

Manifest-backed deployments use an additive `v1alpha1 Connection.auth` shape
that mirrors the local GitHub App identity:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: acme
spec:
  connections:
    - name: product-docs-pr
      type: repo
      provider: github
      auth:
        kind: github-app
        appId: 123456
        installationId: 987654
      secretRef:
        name: product-docs-app-private-key
```

For `provider: github`, omitting `auth` preserves the existing static-token
connection: `secretRef` resolves to the token. `auth.kind: github-app` changes
that meaning unambiguously: `secretRef` is the App's PEM-encoded private-key
reference, and no PAT exists on that connection. The `v1alpha1` `auth` object
has exactly `kind`, `appId`, and `installationId`. `appId` is the non-empty
numeric App ID or client ID string accepted by the existing local
`GitHubID` contract; `installationId` is a non-zero base-10 unsigned integer.
`secretRef` must resolve to a PKCS#1 or PKCS#8 RSA private key. Missing fields,
an unknown auth kind, GitHub App fields on another provider, malformed IDs or
key material, and an inline private key are validation errors. Existing
non-App connections remain byte-for-byte compatible.

`repository.connectionRef` is a direct, case-sensitive lookup by
`Connection.name`; provider matching or list order never selects a connection.
The name must resolve exactly once to `type: repo`, `provider: github`, and the
`github-app` shape above. The runner then synthesizes the target's existing
credential route as follows:

| `RepoBinding` input | Value |
|---|---|
| `Owner`, `Name` | The normalized owner and name from `docsSink.repository` |
| `TokenRef` | An opaque, collision-free resolver ref for this connection and normalized target identity |
| Resolver behind `TokenRef` | GitHub App source configured with the connection's `appId`, `installationId`, and `secretRef`, with `repositories: [repository.name]` |

The opaque ref's spelling is internal; its identity is the tuple
`(connection name, provider, owner, repository)`, so reusing one connection for
another repository creates a different down-scoped token source. The resulting
binding enters the same `RepoScopedCapability` and grant lookup used by
`AdditionalRepos`; it is not a parallel resolver. The exact target identity
supplies `RepoBinding.Owner` and `RepoBinding.Name`—the connection cannot
redirect them—and the mint plus target probe confirms that the declared
installation reaches that repository.

There is no token, credential alias, source override, or `autoMerge` field in
`docsSink`. At local tiers, the exact repository identity selects one
`instance.yaml repos[]` binding. At connection-backed tiers, the exact identity
and `connectionRef` select one compatible GitHub connection. A global
unqualified credential override must not replace either repository-qualified
selection.

`docsRoots` always keeps its existing source-relative meaning. `docs-churn`
runs against the source checkout and emits the existing
`goobers.dev/docs-churn/v1` artifact: its `docsRoots` and `docsRootChanges`
fields contain source paths only. A foreign sink neither changes that schema
nor mixes target changes into those fields. If a foreign-sink workflow omits
`docsRoots` because the source has no documentation tree, both artifact fields
retain their existing empty/omitted behavior.

`docsSink.writeRoots` is a separate ordered list of target-relative files or
directories and is the foreign sink's only write boundary. It is required and
non-empty for `docs-repo`, forbidden for `in-repo`, and has no implicit default
from `docsRoots`; source and target layouts need not correspond. The lists have
no positional mapping. Each list independently uses the existing lexical and
resolved containment rules and is checked for existence in its own checkout.

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
5. The source read binding and target GitHub App write binding must use
   distinct credential identities. The target connection name and
   `secretRef` must differ from the source connection's, and the normalized
   `(appId, installationId)` tuple must differ when the source also uses a
   GitHub App. The equivalent local App tuple and private-key reference must
   differ. Reusing the target App installation for the source is rejected.
6. When `connectionRef` is used, it must resolve, name a GitHub connection,
   satisfy the Manifest GitHub App connection contract above, match the
   repository's provider, and differ from the source connection.
7. `docsRoots` and `docsSink.writeRoots` must each pass the existing lexical
   containment checks. Source roots are checked in the source tree; write
   roots are checked in the target tree.

Same-owner and cross-owner sinks follow the same rules. Sharing an owner does
not permit credential reuse. Crossing owners does not require a special mode:
the exact target binding must independently authenticate to the target owner.
No owner substitution, first-repository default, ambient `GH_TOKEN`, or source
credential fallback is allowed.

GitHub wiki and Azure DevOps wiki identities are not accepted as `docs-repo`
targets. GitHub wiki URL, branch, ownership, and review semantics are defined
by [`github-wiki-docs-sink.md`](github-wiki-docs-sink.md); Azure DevOps wiki
semantics remain a decision for #1021.

## Two-grant capability flow

The credential route for a foreign sink contains two repository-qualified
grants:

```text
contents:read@acme/product
    -> PRODUCT_SOURCE_READ_TOKEN

github:pr:write@acme-docs/product-docs
    -> GitHub App installation 987654
    -> ephemeral token scoped to acme-docs/product-docs
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

Distinct reference names alone are not enough. GitHub App credentials must use
distinct repository-scoped installation bindings. Every target mint is scoped
to the one sink repository and only the permissions required by
`github:pr:write`; the App private key is never exposed to a stage.

## Fail-closed preflight

Foreign-sink admission completes before either repository is checked out and
before any branch, commit, push, pull request, comment, or label mutation:

1. Compile and cross-validate the source, sink, source `docsRoots`, target
   `writeRoots`, exact deployment bindings, local or Manifest `github-app`
   target auth, and distinct credential identities.
2. Resolve the source credential and target App private-key source. For a
   Manifest connection, validate its `auth` fields and parse the PEM returned
   by `secretRef`. A missing env var, unreadable file, failed store lookup,
   malformed key, or invalid App configuration aborts admission.
3. Probe the exact source repository with the source credential and require
   read access to the source revision.
4. Mint the target installation token with `repositories: [target.name]` and
   requested permissions `contents: write` and `pull_requests: write`. Require
   GitHub's mint response to report both permissions at `write`; a refused,
   missing, downgraded, or indeterminate permission response fails admission.
5. With that token, probe the exact target repository and configured base
   branch. Reject an identity, owner, repository, branch, or accessibility
   mismatch.
6. Pin the admitted source and target base SHAs in the run input, then begin
   checkout.

The repository probes are read-only. App-token minting creates only an
ephemeral credential and no repository state. A failed preflight creates no
working copy and performs no repository mutation. In particular, the runner
must not "try the source token" after a target probe fails.

GitHub does not expose the actual repository permission set of a fine-grained
PAT through a read-only introspection endpoint. `X-Accepted-GitHub-Permissions`
describes what an endpoint accepts, not what the presented token has, and a
test push would violate fail-before-mutation. Therefore #1495 must reject a
static PAT as the foreign target credential rather than claim to verify it.
Adding PAT targets later requires a separately designed non-mutating
permission-attestation mechanism; it is not an implementation choice left to
#1495.

## Checkout, change, and publish contract

After preflight, #1495 implements this fixed sequence:

1. Check out the pinned source revision read-only, validate any source
   `docsRoots` for existence and resolved containment, and gather the
   churn/current-tree evidence used by docs-updater.
2. Check out the pinned target base into a separate managed working copy.
   Validate `writeRoots` for existence and resolved containment, then create
   `BranchNamespace + "docs-updater/" + runID` from the target base, not from
   the source revision.
3. Present source evidence as read-only context and make the target checkout
   the only writable repository workspace for the docs agent.
4. Apply and validate changes. Every changed path must remain inside at least
   one target-relative `writeRoot`; an empty diff or an escaping path follows
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

#1495 owns the additive `docsSink` and `Connection.auth` API/schema/deep-copy
surfaces, validation, Manifest-connection-to-`RepoBinding` adaptation,
repo-qualified write-grant routing, GitHub App permission-scoped minting and
preflight probes, dual checkout, target write-root validation, target branch
push, and target PR creation described above. Existing in-repo workflows and
non-App Manifest connections remain byte-for-byte compatible.

This decision does not provision a repository or credential, change
`AdditionalRepos` into writable repositories, add merge authority, or define
Azure DevOps wiki behavior. The GitHub wiki extension is a separate contract
in [`github-wiki-docs-sink.md`](github-wiki-docs-sink.md).
