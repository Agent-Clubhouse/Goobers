# Design: GitHub wiki sink for docs-updater

> **Status:** approved — accepted design contract (2026-07-26); runtime implementation is
> sequenced after #1495.
>
> **Related:** #472 (docs-updater epic), #1016 (`docsRoots`), #1019 and
> [`separate-docs-repository-sink.md`](separate-docs-repository-sink.md),
> #1020 (this decision), and #1495 (foreign docs-repository runtime).
>
> **Architecture:** [`ARCHITECTURE.md` §2 and §5](../ARCHITECTURE.md) and
> [`multi-gaggle-validation.md` §2](v1/multi-gaggle-validation.md).

## Decision

A docs-updater workflow may select its GitHub project's companion wiki as its
single documentation sink. The project repository remains the read-only source
for churn and current-tree evidence. The derived `<repository>.wiki.git`
companion is the only writable workspace.

GitHub wikis have no pull-request or merge surface. Publishing therefore means
committing to the wiki's current default branch and pushing that commit
directly. It never means opening a pull request in the project repository,
creating a feature branch in the wiki, or routing the update through the
project's merge-review workflow.

Direct Git publication does not imply unreviewed publication. Wiki sinks are
human-review-gated by default. An owner may explicitly select unattended direct
publication when configuring the sink. In either mode the Git shape is one
fast-forward commit on the wiki default branch; the difference is whether a
human approves the exact proposed diff before that push.

## Configuration contract

The accepted `Workflow.spec.docsSink` union gains a `github-wiki` form:

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
    kind: github-wiki
    repository:
      provider: github
      owner: acme
      name: product
      connectionRef: product-wiki-publisher
    pageFiles:
      - Home.md
      - Getting-Started.md
      - _Sidebar.md
    publication: reviewed
  # triggers, tasks, and gates omitted
```

The complete sink union is:

| Form | Source | Writable target | Publication |
|---|---|---|---|
| omitted or `kind: in-repo` | `Gaggle.spec.project` | `docsRoots` in that project | Project pull request |
| `kind: docs-repo` | `Gaggle.spec.project` | `writeRoots` in one distinct repository | Target pull request |
| `kind: github-wiki` | `Gaggle.spec.project` | Exact `pageFiles` in that project's companion wiki | Direct wiki commit |

Exactly one form applies. A wiki is selected instead of in-repo or
separate-repository documentation; it is never layered on either. `docsRoots`
keeps its source-relative signal meaning for every form. For `github-wiki` it
is not a write boundary and no source path is mapped positionally to a wiki
page.

### Repository identity

`repository` has a wiki-specific `WikiRepositoryRef` shape:

1. `provider`, `owner`, `name`, and `connectionRef` are required.
2. `provider` has the single allowed value `github`. No other fields are
   admitted.
3. The case-insensitive `(provider, owner, name)` tuple must equal the
   workflow gaggle's project identity, and the normalized GitHub endpoint
   selected by the wiki-publisher binding must equal the endpoint selected by
   the project's source-read binding. An omitted public endpoint and its
   explicit equivalent normalize identically; the same owner and name on
   different GitHub Enterprise hosts do not.
4. `connectionRef` must differ from
   `Gaggle.spec.project.connectionRef` and select the independently scoped
   wiki-publisher credential.

`WikiRepositoryRef` is a distinct closed CRD object; it does not embed or
reuse `RepoRef`. In particular, it has no `project`, `branch`, or `checkout`
property. The `RepoRef.branch` default therefore cannot materialize `main` in
a valid wiki declaration, while supplying any of those fields is an unknown
field validation error.

Repeating the controlling repository identity is intentional: it makes the
credential choice explicit while validation prevents the source and sink
identities from drifting. The Git target is not the declared repository
itself. The runner constructs the target from the shared validated GitHub
endpoint, owner, and repository name as
`<endpoint>/<owner>/<name>.wiki.git`. When the
configured source clone URL is the starting representation, derivation first
removes one terminal `.git` path suffix, if present, and then appends
`.wiki.git`: both `https://github.com/acme/product` and
`https://github.com/acme/product.git` derive
`https://github.com/acme/product.wiki.git`, never
`product.git.wiki.git`. A workflow cannot supply a clone URL, wiki repository
name, or branch.

The wiki default branch is provider-owned. Admission resolves and pins the
symbolic `HEAD` advertised by the companion repository; definitions do not
assume or configure `master`. An absent, disabled, empty, or inaccessible wiki
fails admission. The first `Home` page must therefore be initialized through
GitHub before Goobers can own the wiki.

### Page files and naming

`pageFiles` is a required, non-empty ordered list of exact wiki-root Markdown
filenames. It is the complete write boundary and ownership declaration for the
sink.

- Each value is one filename ending in `.md`, with no directory separator,
  leading dot, `.` or `..` segment, control character, or GitHub-forbidden
  filename character.
- Names are unique under the canonical Gollum page key defined below, not only
  under filesystem spelling.
- `Home.md`, `_Sidebar.md`, and `_Footer.md` are ordinary owned page files and
  may change only when listed.
- An owned page may be created or updated. Page deletion and rename are not
  part of additive docs drift and are rejected.
- Every unowned file is preserved byte-for-byte. Unlisted pages may use other
  GitHub-supported markup or live in nested directories; they remain outside
  the write boundary but participate in the page-identity inventory below.
  Images, attachments, nested page directories, and generated navigation are
  outside the first implementation's mutation surface.

The filename is the page mapping. For example, `Getting-Started.md` owns the
wiki page GitHub renders from that file; no second title, slug, source path, or
position-based mapping exists. This keeps links and ownership stable when the
source documentation layout changes.

Collision validation inventories every renderable page source, not only
`.md` files. A renderable page source is any regular file anywhere in the tree
whose case-insensitive terminal suffix is accepted by the target GitHub wiki's
Markup/Gollum registry. The provider adapter owns an exhaustive, versioned
registry for that deployment; it must include Markdown aliases such as `.md`
and `.markdown`, and the Textile, RDoc, Org, Creole, MediaWiki, AsciiDoc,
reStructuredText, and Pod suffixes accepted by GitHub. Compound suffixes such
as `.rst.txt` and `.rest.txt` are one markup suffix. If the adapter cannot
establish the complete supported suffix set for the target deployment,
admission fails rather than classifying an unknown page as an attachment.

For every renderable page source, the canonical Gollum page key is computed by
removing the longest matching markup suffix, taking the final path component,
applying Unicode case folding, and replacing each maximal run of ASCII spaces
and hyphens with one `-`. Directory components are deliberately excluded:
Gollum's extensionless page resolution may find a nested page by basename.
Thus `Getting Started.md`, `getting-started.markdown`, and
`guides/GETTING--STARTED.textile` have one page identity. Other characters,
including the leading underscores in `_Sidebar.md` and `_Footer.md`, remain
significant.

Admission recursively computes that key for every declared `pageFile` and
every renderable page source in the pinned wiki tree. A key may map to only one
physical repository-relative path across the whole tree, including preserved
unlisted pages. The one allowed match is an owned `pageFile` whose root path
and spelling exactly equal the existing file it updates. A differently
spelled, differently formatted, or nested existing file is not silently
claimed or renamed: declaring `Home.md` collides with either `Home.markdown`
or `archive/Home.textile`, and declaring `Getting-Started.md` collides with an
unlisted `Getting Started.md`. Existing unlisted-to-unlisted collisions also
fail closed because the remote wiki does not have an unambiguous page identity.
The same recursive inventory check runs again on the proposed tree before
review or publication, so an agent cannot create an alias of a preserved page.

## Ownership and overlap

The ownership unit is the whole companion wiki, not an individual page. Across
one loaded config set, at most one workflow may declare `kind: github-wiki` for
the same normalized GitHub endpoint, owner, and repository. Disjoint
`pageFiles` do not make two direct publishers safe: both would still race on
one directly updated default branch and neither repository has a merge queue.

The same project's docs-updater also cannot declare or retain a second
in-repo/docs-repo sink. Existing source documentation may remain in the code
repository and may appear in `docsRoots` as evidence, but the workflow writes
only its selected wiki pages. Maintaining both locations requires an explicit
future mirroring design with one source of truth; two independent docs-updater
owners are rejected rather than allowed to drift.

Config validation can prove ownership only within one instance. Operators must
not configure another instance to own the same wiki. Runtime still protects
manual edits and accidental external writers through pinned-head,
fast-forward-only publication: a changed remote head aborts the push and
requires a fresh run. The sink never force-pushes.

## Capability and credential flow

The wiki sink reuses #1019/#1495's repository-qualified routing, resolver,
redaction, and GitHub App minting. It adds a wiki-purpose write route rather
than making `AdditionalRepos` writable:

```text
contents:read@acme/product
    -> source read credential

repo:push@acme/product (purpose: github-wiki)
    -> product-wiki-publisher GitHub App installation
    -> ephemeral token with contents:write
    -> https://github.com/acme/product.wiki.git
```

The scope qualifier's concrete spelling is internal. Its identity includes the
validated repository and `github-wiki` purpose so it cannot collide with or
replace a source-repository `repo:push` grant. Workflow YAML declares only the
base `repo:push` capability; the runner derives the qualified route from the
admitted sink. An untrusted input cannot redirect it to another wiki.

`connectionRef` follows the accepted foreign-sink connection contract: it
selects an introspectable GitHub App installation binding for the controlling
project repository. The App requires **Contents: read/write** and no pull
request, issue, administration, or merge permission. The source read and wiki
publisher bindings must have distinct connection names, secret references,
and App installation identities. Static PAT wiki publishers are rejected for
the same fail-before-mutation reason as static PAT foreign-repository targets.

At local tiers, #1495's repository routing must expose an equivalent named
GitHub App binding. Exact repository identity alone is insufficient because
the source reader and wiki publisher deliberately refer to the same
controlling repository. Selection by first match, list order, ambient
`GH_TOKEN`, or fallback to the source credential is forbidden.

The local `instance.yaml` schema therefore adds `bindingName` to each
`repos[]` entry. It names a credential binding, not a repository, and uses the
same identifier rules as Manifest `Connection.name`. Binding names are
non-empty, case-sensitive, and unique within the instance:

```yaml
repos:
  - bindingName: product-source-reader
    provider: github
    owner: acme
    name: product
    token: {env: PRODUCT_SOURCE_READ_TOKEN}
  - bindingName: product-wiki-publisher
    provider: github
    owner: acme
    name: product
    auth:
      kind: github-app
      appId: 123456
      installationId: 987654
      privateKey: {env: PRODUCT_WIKI_APP_KEY}
```

`Gaggle.spec.project.connectionRef: product-source-reader` selects the first
binding, while the sink's
`repository.connectionRef: product-wiki-publisher` selects the second. At
local tiers, `connectionRef` is a direct exact lookup by
`repos[].bindingName`; only after that lookup does validation require the
selected binding's normalized `(provider, owner, name)` to equal the declaring
`WikiRepositoryRef`. At Manifest-backed tiers it remains a direct exact lookup
by `Connection.name`, followed by the same provider, identity, and auth checks.

An unnamed local binding remains valid only for legacy identity-selected
workflows when its normalized repository identity occurs once. If an identity
has multiple `repos[]` entries, every entry for it must have a distinct
`bindingName`, and every declaration that uses that identity must carry the
intended `connectionRef`. Missing names, missing references, duplicate names,
identity mismatches, and references that resolve more than once are admission
errors. Neither deployment shape falls back from a missing `connectionRef` to
identity-only selection.

Secret isolation follows the foreign-sink contract:

- Source gathering can resolve only the source read grant.
- Wiki checkout and publication can resolve only the wiki-purpose push grant.
- The docs agent receives the read-only source evidence and writable wiki
  workspace but no repository credential.
- Git authentication is command-scoped and is not persisted in remotes,
  credential stores, artifacts, envelopes, or logs.
- Resolved credentials are registered with the journal scrubber before use.

The App token is scoped by GitHub to the controlling project repository even
though Git transport addresses its derived wiki companion. Goobers further
limits its use to the admitted wiki URL. It must never reuse that token for a
source checkout or source-repository mutation.

## Fail-closed preflight

Admission finishes before either checkout and before any commit or push:

1. Resolve the workflow's gaggle project, exclusive sink kind, exact page
   ownership, publication policy, and non-overlap across the config set.
2. Resolve distinct source-read and wiki-publisher bindings, then validate the
   repeated repository identity and normalized endpoint against the source.
3. Resolve the source credential and target App private-key source. Mint the
   installation token for the controlling repository with requested
   `contents: write`, and require the mint response to report that permission
   at `write`.
4. Probe the controlling repository and require that its wiki is enabled.
5. Probe the exact derived `.wiki.git` URL with command-scoped authentication.
   Resolve a non-empty symbolic `HEAD`, pin its commit SHA, recursively read
   the complete tree at that SHA, and validate canonical page keys against
   owned and preserved pages in every supported markup format.
6. Pin the source revision, wiki endpoint/identity, wiki branch and head SHA,
   page files, publication policy, and credential-route identities in the run
   input, without credential material.

All probes are read-only. Failure creates no working copy and performs no
repository mutation. In particular, the runner does not try a source token
after the wiki credential fails and does not initialize an absent wiki.

## Checkout, change, review, and publish

After preflight, the runtime transaction is fixed:

1. Check out the pinned source revision read-only and gather the existing
   docs-churn/current-tree evidence using source-relative `docsRoots`.
2. Clone the pinned wiki `HEAD` into a separate managed working copy. Detach
   any source remote credentials and make the wiki the docs agent's only
   writable repository workspace.
3. Apply the docs update. Validate that every changed path exactly matches an
   admitted `pageFile`, that no page was deleted or renamed, that unowned files
   are unchanged, and that the resulting recursive page-source inventory
   remains unique by canonical Gollum page key. An empty diff follows the
   existing `no-work` path.
4. Run the workflow's deterministic wiki validation in that same proposed
   tree. The source project's CI command is not implicitly reused because a
   wiki companion need not contain the source build.
5. Materialize a content-digested diff artifact and proposed tree SHA. For
   `publication: reviewed`, pause at a human gate that names this digest.
   Approval is valid only for that exact tree; any later edit invalidates it.
   For `publication: direct`, continue after deterministic validation.
6. Re-fetch the wiki default branch. If its head differs from the pinned SHA,
   abort without rebasing, force-pushing, or overwriting the external change.
7. Create one commit on top of the pinned head and push it as a fast-forward
   update directly to the wiki default branch. Do not push a run branch, invoke
   `open-pr`, or create a project-repository ref.
8. Record the source identity/SHA, derived wiki identity, prior and published
   wiki SHAs, publication policy, approved diff digest when applicable, and
   wiki page URLs as journal evidence. Record no credential material.

Retries are idempotent by run ID, prior wiki SHA, proposed tree digest, and
published commit trailer. If the exact run commit is already the remote head,
publication succeeds without another commit. Any other remote advancement is a
stale-base failure requiring a new run.

## Review policy

`publication` has exactly two values:

| Value | Meaning |
|---|---|
| omitted or `reviewed` | A human approves the exact validated wiki diff before the direct push. |
| `direct` | The validated update is pushed without a human gate. This is an explicit repository-owner opt-in. |

Both policies commit directly because GitHub wikis cannot host pull requests.
There is no `autoMerge` field and no `github:pr:write` or
`github:pr:merge` grant. A reviewed wiki update is not represented as a hidden
project PR: doing so would review a copy while publishing a different Git
object and would blur source-versus-sink ownership.

The compiler requires every path from wiki mutation to publication to pass
deterministic validation. In reviewed mode it additionally requires every such
path to pass the digest-bound human gate after validation. A workflow cannot
route around that gate on an alternate verdict branch.

## Implementation boundary

Runtime work starts only after #1495 has shipped the accepted `docsSink`
foundation, GitHub App connection adaptation, repository-qualified write
grants, and dual-workspace transaction. The wiki implementation then owns the
additive `github-wiki` schema/deep-copy/validation surface, purpose-qualified
credential binding (including local `repos[].bindingName` lookup), derived
companion checkout, page boundary and canonical collision validation,
review-gate validation, direct fast-forward publisher, journal evidence, and
hermetic tests for stale-head and retry behavior.

This decision does not provision or initialize a wiki, permit arbitrary wiki
URLs, make `AdditionalRepos` writable, publish to an Azure DevOps wiki, allow
multiple sink owners, or activate a scheduled docs-updater workflow.
