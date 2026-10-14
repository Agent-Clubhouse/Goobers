# Design: Apply-time gaggle references and portable cloud adoption

> Status: **draft** - proposal for review; no reference or macro syntax below is shipped.
> Area: config-as-code, shared gaggles, Workflow CD, cloud adoption
> Tracking: #3572, #3578
> Verified: ff826744e (2026-10-13)

## 1. Outcome and scope

A product team keeps its complete Foo gaggle in repository A. An operator
enrolls it in a local or cloud instance by committing a small reference, not
copying and independently maintaining the definitions in a workforce repository.
`goobers apply` resolves the reference into an ordinary, validated, immutable
configuration generation. Skills and instructions can be bound or expanded
from explicit package inputs before that generation is admitted.

The reference behaves like a **logical symlink at authoring time**: it names
another package instead of owning a fork. It is **not an operating-system
symlink at execution time**. Applied files are regular files with pinned
contents. Windows privilege requirements, inaccessible local checkouts, and
links escaping confinement must not determine whether a cloud run works.

This proposal extends [Workflow CD](workflow-cd.md) and supplies the source
composition contract requested by #3578. It does not replace the runtime DSL,
the daemon as control plane, or the [Goobernetes mode model](goobernetes-architecture.md).

In scope:

- References to a whole gaggle in a repository subtree, including its workflows,
  local personas, skills, instructions, assets, and package-local scripts.
- Reproducible locks, explicit upstream updates, and opt-in tracked delivery.
- Typed parameters, bounded instruction fragments, and skill-package binding.
- Destination authorization, namespacing, cloud generation availability, and a
  safe local-to-cloud adoption/cutover story.
- Shared primitive identity and the extension boundary for CI/setup contracts.

Not in scope: actual filesystem links, arbitrary template evaluation, plugin
execution during resolution, automatic credential provisioning, sharing mutable
instance state, moving active runs between execution engines, or zero-downtime
local-to-Temporal migration. This PR is design only.

## 2. Existing contracts and the gaps

The following is grounded in the source at the header's revision, not inferred
from whether an old issue is open or closed.

| Existing surface | Reuse | Missing contract |
| --- | --- | --- |
| `workflowSource` / `GitSource` (`internal/instance/config.go`, `gitsource.go`) | Read a committed Git tree; isolate source credentials from stage grants. | One root source, branch selection, no subtree selector or dependency graph. |
| `goobers apply` / `configsource_apply.go` | Live-daemon reconciliation, validation rejection, last-known-good restoration. | Resolve dependencies before publication; notice tracked dependencies when the root revision has not changed. |
| [Tracked gaggle templates](../guides/gaggle-templates.md) | Self-contained packages, typed name rebinding, provenance, reviewable explicit updates. | Creates an owned editable copy and performs three-way merging, not an ongoing reference. No transitive dependencies. |
| [Portable gaggle bundles](../guides/gaggle-bundles.md) | Complete definition/file validation and destination authorization; no copied runtime state. | Snapshot import, not repository subscription. Sanitized bundle restrictions are not a general source-package format. |
| [Instance-shared goobers](../guides/shared-goobers.md) | Existing persona and skill scope rules. | Shared names are not permission to shadow a persona or fetch another repository implicitly. |
| [Run configuration generations](../guides/run-config-generations.md) | Immutable retained execution inputs, digest verification, crash recovery and current authority checks. | Preserve resolution provenance and verify every configured execution path can obtain the resolved generation. |
| [Rendered config mirror](../../deploy/reference/config-mirror/README.md) | Credential-free worker boot seed. | Boot seed is not live update or proof that a worker can execute a newly admitted generation. |

`internal/harness/skills.go` already materializes declared skill packages into
harness-native directories; #2230 is closed. The older no-op description in the
shared-goober guide and open #2221 are not grounds for designing a second skill
delivery mechanism. Use the existing skill loader, identity, collision checks,
and materializer with the **resolved** bytes.

Likewise #4153 documents real daemon/worker seed divergence, but generation and
execution-kit paths now exist. Its open state is not proof that every modern
run still depends on a worker's startup tree. The implementation must prove
generation availability for each path rather than mandate worker restarts for
all definition updates or claim that the historical failure is universally fixed.

## 3. Authoring contract: a reference, not a copied gaggle

All YAML in this section is **proposed source syntax**. Runtime APIs and CRDs
continue to receive resolved `Gaggle`, `Workflow`, and `Goober` documents.

### 3.1 Instance provisioning authorizes sources

Add an optional, named `configSources` inventory to `instance.yaml`, separate
from both `workflowSource` and product `repos[]`. Example:

```yaml
configSources:
  - name: foo-config
    kind: git
    url: https://github.com/acme/foo.git
    allowedDirectories: [goobers/gaggles/foo]
    token:
      env: FOO_CONFIG_READ_TOKEN
  - name: shared
    kind: git
    url: https://github.com/acme/workforce-library.git
    allowedDirectories: [skills, instructions]
    token:
      env: SHARED_CONFIG_READ_TOKEN
```

The inventory authorizes reads; it grants **no target-repository write access**.
Reuse the existing token-reference and GitHub App authentication shapes, including
their mutual exclusion. A GitHub App credential must be down-scoped to the
selected repository. Local development may use `kind: git` with `path` instead
of `url`, and no Goobers-supplied credential; it reads committed objects, never
the current checkout. Host-local source paths are not portable cloud enrollment.

Remote URLs are explicit HTTPS repository identities, with no userinfo, query,
fragment, credential helper fallback, or submodule/LFS fetch. Require an
explicit read identity as existing remote workflow sources do. An undeclared
source, directory outside the allowlist, redirect to another origin, or denied
repository access is an error. Package content cannot add sources, change their
credentials, or request a different URL through a parameter.

Provisioning remains restart-only. Adding a source to a running daemon must
report the required restart, not infer new source authority from config content.
Existing configs without this inventory behave as before.
Source syntax is untrusted executable configuration: an operator must also
explicitly authorize `updateMode: tracked` for a source in provisioning
(`allowTracked: true`, default false). A destination descriptor cannot grant
itself automatic upstream-change authority by changing its own update mode.

### 3.2 Destination reference

The root source manifest still lists the destination gaggle:

```yaml
spec:
  gaggles: [foo-cloud]
```

Instead of a normal definition tree, it contains
`gaggles/foo-cloud/reference.yaml`:

```yaml
apiVersion: goobers.dev/source/v1alpha1
kind: GaggleReference
metadata:
  name: foo-cloud
spec:
  source: foo-config
  ref: main
  directory: goobers/gaggles/foo
  updateMode: locked
  enabled: false
  parameters:
    ciCommand: ["dotnet", "test"]
    reviewFocus: "Compatibility and user-visible failure handling."
  bindings:
    project:
      provider: github
      owner: acme
      name: foo
      branch: main
    backlog:
      provider: github
      project: acme/foo
    isolation:
      namespace: gaggle-foo-cloud
```

The namespace/version above identifies a **source-only** schema, not a new
Kubernetes resource. Exact public vocabulary is subject to maintainer review.
Do not accept it until schema, CLI, source discovery, and validation support
land together. Unknown fields and unsupported source versions fail closed.
Older binaries must reject an unresolved reference rather than silently omit
the named gaggle.

Contract:

- A manifest name selects exactly one inline gaggle or one reference. Both,
  duplicate names, unregistered references, or an unresolved manifest name fail.
  A reference directory contains only the descriptor; customized ordinary
  definitions belong in inline/template mode, not beside an active reference.
- `directory` is a normalized repository-relative package path. It may select
  a gaggle under repository A without making repository A the root
  `workflowSource`. It must be non-empty, not `.` or the repository root; use
  a dedicated package subtree. The ordinary root-source contract stays unchanged.
- The selected directory contains exactly one gaggle and its complete package
  closure. A `.gaggle-package.yaml` declaration is optional for a self-contained,
  unparameterized existing gaggle; parameters/dependencies require it. Exclude
  template enrollment/runtime metadata from the executable package and reject
  instance roots, journals, workcopies, `instance.yaml` and nested root manifests.
  Referencing a template author's definitions must not enroll the destination
  into both template-copy and reference ownership.
- `ref` selects an explicit branch, tag, or full Git object ID. Resolution records
  the full commit; never execute a branch/tag label as a pin. Annotated tags are
  peeled to a commit. Abbreviated object IDs and ambiguous ref names are refused.
- `updateMode` defaults to `locked`. `enabled` defaults to `false`, independently
  of the upstream gaggle's enabled setting. Enrollment must not start work on
  the author's target before the destination has authorized it.
- `bindings` uses existing typed project/backlog/isolation structures. It does
  not accept credentials, `instance.yaml`, arbitrary JSON patches, or a generic
  recursive YAML merge. Project, backlog and isolation bindings are required
  even when they intentionally match the upstream values. Destination
  repository authorization is checked after binding. Optional
  `bindings.additionalRepos` replaces the complete typed additional-repository
  list when changing those targets; none is silently dropped or authorized by
  an inherited source credential.
- Preserve workflow names within the destination gaggle. Bind `spec.gaggle` and
  prefix imported local persona names with the destination gaggle name, using
  the tracked-template binder and its collision checks. Rewrite only typed
  references, never names hidden in scripts, prose, or opaque task strings.
  Reject global persona name collisions; do not invent local-over-shared wins.
- Imported personas remain gaggle-local. A shared upstream persona/skill must
  be explicitly packaged or declared as a dependency and localized into this
  gaggle, not implicitly install itself as an instance-wide persona.

### 3.3 Ownership and alternatives

| Mode | Who owns the effective content? | Updating it |
| --- | --- | --- |
| Inline gaggle | Destination source | Ordinary source edits and apply. |
| Tracked template | Destination owns its copy/customizations | Explicit three-way template update; existing backprop rules. |
| Locked reference | Upstream package; destination owns bindings and accepted lock | Review a lock update, then apply. |
| Tracked reference | Upstream package; destination explicitly delegates changes on the selected ref | Reconcile and validate a new resolution before admitting new runs. |
| Bundle import | Destination owns a snapshot | Import a new, separately reviewed copy; no subscription implied. |

Do not silently enroll existing copies or convert template metadata into a
reference. Changing ownership is an explicit reviewed source change.

### 3.4 Proposed operator flow and resolved shape

The following is a future command flow, **not runnable on current releases**:

```sh
goobers config references lock --source ./cloud-config ./cloud-instance
goobers validate --source-tree ./cloud-config
# Review and commit reference.yaml plus .references/lock.json through a PR.
# Merge that PR to the cloud instance's configured workflowSource ref.
goobers apply ./cloud-instance
```

The disabled enrollment produces ordinary resolved files such as
`config/gaggles/foo-cloud/gaggle.yaml`,
`config/gaggles/foo-cloud/goobers/reviewer/goober.yaml`, and
`config/gaggles/foo-cloud/skills/review/SKILL.md`. The gaggle has
`metadata.name: foo-cloud`, `spec.enabled: false` and destination bindings;
workflows declare `spec.gaggle: foo-cloud` and typed persona references select
`foo-cloud-reviewer`. As in the template binder, local file paths stay stable
while typed identities change. No unresolved `GaggleReference`, typed parameter
node, credential value or filesystem link reaches the runtime interpreter;
intentionally escaped macro-looking instruction text stays literal.

With the sample parameter, the resolved instruction starts with
`Review the Foo changes for Compatibility and user-visible failure handling.`
followed by the exact locked `policy.md` bytes. That rendered instruction and
the rendered skill files participate in persona/configuration identity.
Enabling the destination is a separate reviewed change after topology and
cutover verification.

## 4. Locks, reproducibility, and updates

There are two distinct records:

1. **Source lock**: `<source-root>/.references/lock.json`, reviewed and committed
   with the destination source for locked entries.
2. **Applied resolution manifest**: secret-free metadata captured with the
   immutable configuration generation for every reference, including tracked
   entries. It records what actually ran, not an assertion that it was approved
   in a source lock.

The proposed source-side command
`goobers config references lock --source <checkout> <instance-root>` resolves
initial locked entries using the authorized source inventory and writes a
candidate lock atomically. An explicit `--update <gaggle>` advances only the
selected entry and its dependency closure. It validates the full candidate,
prints old/new revisions, rendered changes and authority changes, but never
deploys, commits, pushes, or writes the upstream repository. Source edits use
the existing source-edit lock; modifying a running instance is not required.
Tracked entries are not silently converted to locked by this command.
Preparation also resolves tracked entries into the verified cache for a
complete offline validation, but does not write their candidate commits as
accepted source pins. An all-tracked source need not contain a source lock.

A normal `goobers apply` consumes the accepted locked graph. It may fetch
missing locked objects but **cannot move pins or write into a source checkout**.
A missing/mismatched source lock for a locked entry is an error, even on first
apply. Offline validation may use a verified complete cache; absent objects produce an explicit
unresolved error, never an empty package or a successful partial validation.
Read-only status/diff inventory does not fetch.

Each lock entry includes:

- Source alias plus normalized repository identity, requested ref, full commit,
  selected directory, source contract version and resolver semantic version.
- Package content digest, including paths and executable modes; dependency
  edges and each dependency's exact source/commit/directory/content digest.
- Destination gaggle/name mapping, effective non-secret bindings/parameters
  digest, and final rendered package digest.

Do not store credential values, resolved environment values, absolute local
checkout paths, or a full provider/private-key error in either record.
Persist source aliases and logical repository identity, not provisioning secrets.
Time of resolution is audit metadata and excluded from content identity.

The lock is a **vector of exact commits**, not a distributed snapshot of several
repositories. It is internally consistent because the resolver fetches each
selected object once and renders only those objects. It must not claim that all
repositories were observed at one global instant.

Source changes to alias identity, selectors, parameters, dependency declarations,
or binding semantics invalidate the matching lock entry. Tampering with bytes or
modes at a locked commit is refused. A previously accepted full commit remains
usable from a verified cache after a branch force-push; an update across rewritten
history requires explicit provenance review, not automatic tracking. If a
source check or update observes a moved tag, reject it pending a reviewed source
selection change; locked offline apply does not need to consult a live tag.

### Tracked delivery

`updateMode: tracked` is explicit delegation to a branch, not the default. Tags
and object IDs are immutable selectors and cannot use tracked mode. The daemon's
reconcile loop observes every tracked root/dependency ref even when the root
workforce repository is unchanged. The resolved graph digest, not only the root
Git SHA, determines whether there is a new candidate. Apply and automatic
reconciliation use the same resolver.

There is no independent dependency `updateMode`: dependencies inherit their
owning reference's mode. Branch dependencies advance with a tracked root's
reconcile sweep; immutable dependencies remain fixed. A change produces one
whole-instance candidate, never several partially updated gaggles.

Auth/fetch/validation failures keep the active last-known-good generation, emit
a coded failure and nonzero explicit-apply result, and mark source status stale
or rejected. Do not report "up to date" because an old cache is readable.
For explicit apply, a tracked-source outage is a failure even if root content
did not change; already admitted runs can continue from retained generations.

## 5. Package inputs, skills, and instruction macros

### 5.1 Optional package declaration

An upstream `.gaggle-package.yaml` is source metadata, removed from executable
definition discovery after its validated provenance is captured. Example:

```yaml
apiVersion: goobers.dev/source/v1alpha1
kind: GagglePackage
metadata:
  name: foo
spec:
  contractVersion: "1.0"
  parameters:
    ciCommand:
      type: string-list
      required: true
      maxItems: 16
    reviewFocus:
      type: string
      default: "Correctness and regressions."
      maxLength: 512
  structuredTemplates:
    - gaggle.yaml
  textTemplates:
    - goobers/reviewer/instructions.md
  dependencies:
    review-skill:
      kind: skill
      source: shared
      ref: refs/tags/v1.2.0
      directory: skills/review
      installAs: review
    review-policy:
      kind: instructions
      source: shared
      ref: refs/tags/v1.2.0
      directory: instructions/review
      file: policy.md
```

Whole-gaggle references are top-level enrollments, not nested gaggle inheritance.
Within a package the first dependency categories are skills and instruction
fragments. Each external dependency names an authorized source and an exact
selected subtree; each is part of the same lock graph. Unrelated upstream
manifest entries and other repository files are never imported.

First-rung dependencies are self-contained: skill/fragment packages cannot
declare further external dependencies. Detect cycles and report the source
chain rather than recurse. A later primitive-package format may add bounded
nested dependencies only with explicit format support, never by interpreting
arbitrary YAML as a package declaration.

### 5.2 Typed replacement in structured definitions

In an explicitly listed structured template, a complete mapping node
`{$param: ciCommand}` is replaced by the declared parameter's typed value:

```yaml
# gaggle.yaml in the source package, before resolution
spec:
  ciCommand: {$param: ciCommand}
```

That becomes an ordinary `spec.ciCommand: ["dotnet", "test"]` list before runtime
schema validation. Supported first-rung types are string, boolean, bounded
integer, and bounded string-list. Required/default/enum/range/length rules are
closed and enforced before substitution. Undeclared caller parameters fail.
Defaults apply only when omitted, not when an explicit value is invalid/null.

No partial scalar interpolation in YAML, substitution in keys, evaluation,
conditionals, loops, arbitrary property access, environment lookup, command
execution, or YAML text concatenation. A parameter is data, not a source URL,
include path, credential, runtime expression or second-stage macro. Values are
never recursively expanded.

Bindings are applied after package parameters and typed identity rebinding.
The destination binding is authoritative for project/backlog/isolation and
enablement. Ordinary package parameters must not override those fields or
credential/authentication references. Lists replace atomically; there is no
implicit append/union or last-file-wins policy.

### 5.3 Instruction expansion

In an explicitly listed UTF-8 text template:

```markdown
Review the Foo changes for {{goobers.param.reviewFocus}}.

{{goobers.include.review-policy}}
```

`goobers.param.<name>` inserts one string value; non-string parameters are an
error in text. `goobers.include.<name>` inserts the bytes of the declared
instruction dependency. Fragments are UTF-8, bounded, and are **not recursively
expanded**. Parameter values and fragments cannot smuggle new includes into
the resolver.

Replacement is exact-token, one-pass substitution, not regex replacement,
shell expansion, Jinja/Go templates, or a general macro language. A reserved
token written directly in a template must resolve; unknown, malformed or
unterminated tokens fail with the file and token location, not input values.
Use `{{goobers.literal.param.name}}` to emit the literal
`{{goobers.param.name}}` (and equivalently `literal.include.name`). Ordinary
`${...}`, `${{ ... }}`, Markdown braces and workflow runtime expressions are
untouched. Only the reserved syntax in declared templates is processed.

Literal output from an escape, parameter, or fragment is never rescanned.
Diagnostics distinguish unresolved tokens in original template bytes from
intentional literal bytes in inserted content.

Instruction substitution is **not an authority boundary**. An upstream
instruction can steer an agent, and a skill may contain executable scripts.
All package and rendered changes remain executable-configuration review;
capability, gate, confinement and provider authorization remain independently
enforced. Do not claim that prompt templating sanitizes untrusted instructions.

### 5.4 Skill and instruction overrides

A skill dependency is copied to the gaggle-local `skills/<installAs>/` tree
before loading personas. A persona still declares the normal bare
`spec.skills: [review]`. The existing loader fingerprints the rendered files,
and the existing harness materializer delivers that pinned package; do not
inject duplicate skill contents into the prompt or overwrite tracked target
repository skills.

A text template may also be an explicitly listed Markdown file inside a skill,
including `SKILL.md`. Only the owning package's declared parameters/fragments
are in scope. Script files and opaque assets cannot opt into text expansion in
the first rung; parameterized executable commands require typed command
contracts, not string injection.

There is no implicit override of an upstream skill or instruction file. A
package may declare a dependency name `replaceable: true`; the destination
reference may then supply `spec.dependencyBindings.<name>` with another allowed
source/ref/directory of the same kind. Retain the package-declared `installAs`
or fragment `file` contract; the caller cannot change the output name/path.
That replacement is locked, attributed,
and validated like the original, and completely replaces it. Unknown or
non-replaceable dependency names fail. Duplicate `installAs` names, package-local
skill/dependency collisions, or overwriting an instruction template fail.

For example, only if `review-skill` is declared replaceable:

```yaml
spec:
  dependencyBindings:
    review-skill:
      source: shared
      ref: refs/tags/v2.0.0
      directory: skills/review
```

`structuredTemplates` and `textTemplates` are explicit package-relative output
paths, including installed dependency paths such as `skills/review/SKILL.md`;
no glob, traversal or aliases are allowed. An external skill's own metadata
cannot opt itself into expansion: only its owning gaggle package can declare
which Markdown files use that package's parameter/fragment scope.

Resolution precedence is fixed: package defaults -> declared caller parameters
and allowed dependency replacements -> typed name rebinding -> destination
bindings -> full validation. No permission/gate/restriction weakening is provided
by a generic overlay. A team needing arbitrary workflow/prose editing should
use an inline gaggle or a tracked-template copy instead.

## 6. One resolution pipeline, every delivery path

The implementation should extract a shared resolver behind the existing
source-materialization seam, not create an independent cloud preprocessor.

1. Read one stable root source snapshot, the matching source lock, destination
   provisioning and source authority. Validate source syntax without fetching.
2. Discover references, authorize sources/selectors, and build the bounded graph.
3. Acquire missing objects without holding the live publication lock. Deduplicate
   fetches by normalized source identity/commit. Read Git blobs directly; never
   checkout and execute Git hooks, filters or source-provided setup commands.
4. Verify selected bytes/modes and locks. Stage the complete closure as regular
   files; resolve typed parameters, fragment/skill bindings and identity mapping.
5. Validate the whole instance: definitions, supported DSL pins, referenced
   instructions/skills, all provider/connection targets, duplicate identities,
   current grants/restrictions, and declared runner inventory. Missing skills
   are errors here even where legacy inline validation warns.
6. Compute the resolution manifest and configuration generation. Publish the
   generation to the configured generation transport, verify availability, and
   retain it before exposing new admissions.
7. Under the existing config/publication locks, recheck root snapshot, accepted
   lock, provisioning authority and active-generation compare-and-swap token.
   If any changed, reject/retry with a bounded attempt; never publish work
   authorized under an obsolete source inventory.
8. Activate the complete candidate using the existing config transaction and
   scheduler reload. Record active generation and applied graph together.
   New runs pin it; existing runs retain their original generation.

The resolver must be used by `goobers apply`, source startup/bootstrap, Git
reconciliation, local `config materialize`, reference-aware `validate`, and
`config-sync` rendering for any retained operator/CRD delivery path. Workers and
stage pods **never** resolve source references, fetch config Git repositories,
perform macro expansion, or independently decide what `main` means.

`validate --source-tree` performs closed source/lock checks and complete cached
resolution. Unresolved references are a failure with an actionable lock/fetch
instruction; validation does not silently validate only inline gaggles.
Source lock generation is the explicit network-enabled preparation step.
Runtime `validate` sees only ordinary resolved definitions and provenance.

Raw edits to materialized referenced files are drift, not upstream changes.
Watchers must reject them and identify the owning reference; they cannot
silently detach ownership or recompute accepted provenance. The same integrity
check applies if inline files and reference output share a materialized tree.
Tutor proposals target the owning source, a declared parameter/dependency
binding, or a reviewed lock update. The Tutor cannot edit generated files or
gain upstream write authority merely because the resolver can read them.

### Topology and partial failures

Adding/removing a gaggle or changing boot-pinned engine/runner provisioning is
not made live merely by introducing references. Return `restartRequired` with
a separately staged pending candidate when the existing topology cannot reload;
the active generation, authority and admission view remain unchanged until
restart. Refuse removal while retained work requires that gaggle's topology
unless the existing runtime explicitly supports its continued recovery.

Source resolution and validation must finish before live publication or any
external side effect. On failure discard only the named staged candidate,
release its leases, and retain last-known-good. On cold start with no accepted
generation, fail startup explicitly.

The existing `applyPreparedConfigSource` path can report divergence after
non-transactional runtime side effects; this design does not claim to roll those
back. Integration must preserve that explicit failure state or eliminate the
side effect with a proven transaction. A crash after generation publication
but before activation leaves an unadmitted candidate, never mixed old/new
definitions. Recover the existing transaction before another apply.

## 7. Cloud convergence and limits

The executable truth is the **resolved generation**, not each pod's boot seed.
Use [retained configuration generations](../guides/run-config-generations.md)
for full-config worker paths and pinned execution kits for stage pods. A mirror
or ConfigMap remains a bootstrap mechanism, not the authority for an admitted
run. Do not broaden the public artifact plane to expose full configuration.

Generation-aware directory workers already require the fleet generation
namespace to be reachable. An endpoint-only artifact worker is not automatically
proof of full-config generation transport: prove an authorized generation path
for it or fail the topology preflight with an explicit unsupported-path error.
Any new generation-read route needs a distinct worker principal and scope;
do not give stage pods full config, source credentials, or another gaggle's files.

Missing, corrupt, wrongly attributed or unreachable generation content refuses
execution with a bounded infrastructure failure. No fallback to current disk,
the upstream ref, or the next worker seed. Keep current operator revocation
checks distinct from historical execution pins; a retained reference cannot
resurrect revoked merge authority.

Apply success means candidate acceptance and generation publication, not that
every scaled-to-zero runner is online. Topology preflight establishes the
configured transport; attempt-time checks establish reality. Expose pending,
active, rejected, stale-source and execution-unavailable states separately.
A deployment still using legacy seed-only execution must drain/reseed/recreate
workers safely or upgrade before claiming reference-driven live convergence.

Proposed resolver limits are rejection thresholds, not truncation:

| Resource | First-rung bound |
| --- | --- |
| Top-level references per instance | 64 |
| Unique packages in the dependency graph | 256 |
| Dependency depth | 2 (gaggle -> self-contained skill/fragment) |
| Whole-gaggle package | 2,048 files / 16 MiB, matching tracked templates |
| Parameters per package | 64; string at most 8 KiB, list at most 64 items |
| One instruction fragment or rendered text file | 1 MiB |
| Complete rendered generation | Existing generation limits, including 4,096 entries, 8 MiB/file and 32 MiB content |
| Resolution sweep | Two minutes, with context cancellation and bounded retry |
| Resolver cache | 256 MiB / 256 package entries, including mirrors and extracted objects |

Both pre-expansion and post-expansion sizes count. A package below its own bound
may still exceed the generation limit; refuse the entire apply. Reject symlinks,
special files, path traversal, absolute paths, case-folded/Windows path aliases,
duplicate output paths, archive bombs and files whose modes cannot be represented
in the existing portable generation contract. Never follow link text flattened
by Windows Git as if it were a regular allowed source file.

Fetch into a bounded disposable mirror and verify Git object-store disk growth,
not just exported package size. Refuse acquisition before promoting an
oversized mirror; document that very large product repositories may need an
operator-prepared local mirror or dedicated config repository. Do not advertise
arbitrarily large repository-A support behind an unbounded full clone.
Git entry mode `160000` (submodule) and LFS pointer content required by a package
are unsupported and must be rejected explicitly, not treated as complete
definitions/assets after skipping their external fetch.

Prune caches through a production caller after resolution and at startup.
Active applies hold leases; durable generations are governed by their existing
run/owner retention rather than depending on a disposable Git cache. Reclaim
only unleased verified cache entries; if all space is protected, refuse the
new resolution. Failed/abandoned temporary fetches are included in the budget
and cleaned through the same bounded recovery path.

## 8. Provenance, telemetry, and drift

Extend generation provenance rather than invent a second run identity:

| Identity | Meaning |
| --- | --- |
| `configGeneration` | Exact applied executable tree retained by the run. |
| `sourceRevision` | Root workforce source revision, when Git-backed. |
| `resolutionDigest` | Canonical full reference graph, selectors, bindings and rendered digests. |
| `referenceId` | Destination gaggle/reference name. |
| `primitiveId` / `primitiveKind` | Package-relative exported primitive/dependency and category. |
| `primitiveVersion` / `packageDigest` | Full source commit plus exact content identity. |
| gaggle / workflow / stage / run / attempt | Existing execution correlation. |

Journal/read-model provenance links a used goober, skill or fragment to the
resolved source package. Success/failure/rejection are distinct outcomes;
resolution rejection can name reference/workflow/stage where known but **must
not fabricate a run ID before admission**. Repository credentials, parameter
values and file contents are never metric labels or rejection logs.
Keep source URLs/commits and digests in structured provenance, not unbounded
Prometheus label cardinality. Inline definitions retain existing telemetry.

Status/diff shows source authority, selected/locked/applied revisions, rendered
changes, active vs pending generation, last successful source check and coded
failure. Resolver diagnostics include the reference/dependency chain and safe
path/field, never fetch stderr containing credentials or instruction values.

For #3581, a reference output's mismatch with its recorded rendered digest is
strict drift and fails validation/apply. Allowed parameters/replacements are
part of the digest, not drift. Do not guess that an unrelated inline script is
a forbidden copy by textual similarity: copied primitive drift requires
declared ancestry/identity, as tracked templates already do.

## 9. Foo migration and rollback

There are two different operator intents, not one destructive "move" operation.

### Adopt definitions into a shared cloud instance

1. Keep repository A's definitions canonical. Make its gaggle subtree a
   self-contained package or explicitly declare its shared skill/instruction
   dependencies. No product-source deletion is required.
2. The cloud operator authorizes source reads, destination product access,
   namespace/identity, engine and runner requirements. A workflow author cannot
   grant these by committing a reference.
3. Commit a disabled reference and accepted lock to the cloud workforce repo.
   Resolve/validate the cloud candidate, inspect source/rendered/authority diffs,
   and perform any declared topology restart before enabling work.
4. Enable only after cutover or deliberately disjoint backlog routing. Do not
   run local and cloud schedulers against the same work without proven shared
   coordination; labels alone have a residual race window.
5. Smoke-test real execution, generation/kit availability on fresh workers,
   project/backlog routing and current-authority checks. Retain the local
   instance snapshot and disable its supervisor before considering retirement.

This creates a new gaggle in the cloud instance: it does **not** import the
local instance ID, claims, journals, running work, credentials or telemetry
database. Historical local evidence remains accessible in the retired instance.
A cloud instance already owns its identity and state.

### Relocate an existing instance

For a same-runtime host move, use the existing
[cold migration runbook](../guides/move-local-instance.md): finish active work,
stop source writers, preserve identity/history/claim state, reprovision secrets,
and verify the destination before retiring the source. Configuration references
do not change the rules for copying durable state or excluding workcopies.

A local-to-Temporal conversion changes recovery/engine topology. This proposal
does not promise resumable engine-history conversion. Finish active local runs
and perform a cold cutover; migrate historical state only through a separately
specified compatible import/restore contract. Do not reuse a local identity in
an existing shared cloud instance or copy its claim ledger over that instance.

### Rollback and detachment

Before cloud work starts, leave the cloud gaggle disabled and restart the
unchanged local instance. After cloud work starts, drain/stop its gaggle, reconcile
provider-visible claims and side effects, then re-enable one authoritative
instance. The old local snapshot is not automatically current after cloud writes.
If cleanup is unavailable, remain stopped and report the required operator
action; do not claim rollback complete.

A source rollback restores a reviewed descriptor/lock and resolves its exact
generation; it cannot downgrade incompatible runtime schemas or undo provider
effects. Tracked references can be changed to locked at a reviewed known-good
full commit. Retained runs keep their own generation.

To detach from upstream, explicitly materialize the accepted package as an
inline/template-owned source in a review branch, retain ancestry, remove the
reference in the same change, validate, and deploy. No automatic "local edit
means detached" behavior. Backprop of generated reference output is refused;
offer the owning parameter/source location or this explicit detach route.

## 10. Compatibility and design review decisions

Source composition has its own versioned contract, independent of workflow
`dslVersion` and instance schema version. New binaries accept old inline sources;
old binaries may execute a compatible **fully rendered** tree but cannot resolve
new source descriptors. Do not downgrade a reference-owning deployment onto a
binary that ignores ownership/lock enforcement.
The provisioning extension must join the existing instance schema-version
gate and closed JSON-schema drift checks. Declare the minimum supported binary
and schema revision when implementation lands; do not admit `configSources`
through a permissive unknown-field decoder on an older schema.

Package `contractVersion` major changes invalidate bindings/parameters until
reviewed; minor additions must preserve accepted input meanings and output
identity rules. Locks include resolver semantics so a change to expansion,
scope or name-binding cannot silently reproduce a different generation.
Each referenced workflow keeps its existing DSL pin; incompatible DSLs,
capabilities, OS assumptions or instance-root-only commands fail the destination
preflight rather than being rewritten into cloud equivalents.

| Reviewed alternative | Decision and consequence |
| --- | --- |
| OS symlink or Git submodule | Reject as the public execution model: privileges, link escape, recursive fetch/auth and host paths are not portable authority. Use typed logical references. |
| Copy everything into Goobernetes-Workflows | Keep supported as inline/template mode, not a prerequisite for cloud adoption. |
| Add `workflowSource.subdir` alone | Insufficient for a shared instance enrolling multiple product repos; keep one root authority and compose gaggles underneath it. |
| Floating `main` everywhere | Reject by default; locked is reviewable and replayable, tracked explicitly delegates upstream change. |
| Worker-side Git sync | Reject: N source identities, independently selected commits, and the #4153 wrong-authority failure shape. |
| Arbitrary macro/overlay language | Reject: code execution and hidden authority changes; typed parameters and one-pass text fragments satisfy the first use cases. |
| Generic workflow-fragment merge now | Defer: graph node names, outputs and capabilities need typed primitive contracts, not deep merge. |
| Claim live migration because config reloads | Reject: definition portability is separate from engine history, claim ownership and cutover. |

The selected first rung is whole gaggles plus skills/instructions. #3580's
CI-command/setup primitives follow the same source/lock/provenance rules, but
require typed input/output/capability contracts and graph/reference binding
before execution. They must expand to ordinary deterministic tasks; loose
repository scripts remain supported and are never executed during resolution.
No text macro is permission to turn an instruction dependency into a setup hook.

Maintainer review must ratify the source-only schema/version, source authority
inventory, opt-in tracked policy, bounded acquisition limits, and permitted
dependency replacements before implementation begins. These are proposed
decisions, not unspecified implementation choices.

## 11. Delivery plan and acceptance evidence

Do not close the implementation epic with a design PR or mark this page
implemented when only the resolver lands.

| Rung | Existing tracking / scope | Depends on | Exit evidence |
| --- | --- | --- | --- |
| A | #3578: ratify source, lock, parameter, trust and migration contract | Maintainer design review | Agreed syntax/examples and compatibility decisions; no runtime implementation in this PR. |
| B | #3579: source schemas, authority wiring, whole-gaggle resolver, locks, every materialization/validation caller | A | Same locked Foo package resolves locally and in cloud; inline/template/bundle regression cases; no partial publication. |
| C | Skills/instruction dependencies and bounded expansion; a separately scoped child of #3572 before coding | B | Exact typed/text output and real harness-native skill consumption; no expansion during execution. |
| D | #3581 drift and #3582 attribution | B; C for fragment/skill cases | Deterministic drift failure and provenance/telemetry shape across references and ordinary runs. |
| E | Cloud transport/topology convergence and cutover drill; explicitly scoped with #4153's current paths | B, C | Fresh worker and stage pod execute old/new pinned generations; disabled enrollment, restart-required topology and rollback drill. |
| F | #3580 typed CI/setup contracts, then #3583 adoption | B, D; adoption also requires E | Canonical shared command executed without copying; safe migration of existing workflows. |

Before creating implementation issues, align their scope with existing children;
do not duplicate #3579/#3581/#3582 with competing resolution designs.

Required behavioral fixtures/drills:

| Scenario | Required result |
| --- | --- |
| Same reviewed graph applied twice, including Linux/Windows | Identical rendered bytes/logical modes and generation identity; no fetch-driven pin movement. |
| Two destinations reference one Foo package | Independent gaggle/persona identities, targets and credentials; no cross-gaggle skill shadowing. |
| Locked ref's branch moves; tracked dependency moves with root unchanged | Locked stays pinned; tracked creates one new candidate or clearly rejects it. |
| Unknown source, unauthorized subtree, changed alias, tag rewrite, malicious redirect, inaccessible object | Coded rejection and nonzero apply; no broad credential fallback or partial gaggle. |
| Invalid params, null, list/scalar mismatch, unknown macro, literal macro, included macro-looking content | Exact defined expansion/escape behavior; values never recursively execute or fetch. |
| Skill replacement/collision, missing instruction/skill, symlink/alias, executable script placeholder | Explicit validation error; target repo content and active generation untouched. |
| Expansion/file/graph/cache/generation bounds exceeded | Full rejection at the stated bound, no truncation, tested wired cleanup/pruning. |
| Upstream force-push, unavailable source, corrupt lock/cache | Preserve verified old generation; never report stale source as up to date. |
| Apply races root/lock/source-authority change; crashes before/after publication | One coherent active generation, recovered transaction, no obsolete-authority activation. |
| New gaggle under boot-pinned engine topology | Disabled/pending enrollment and explicit restart requirement; no success-shaped live activation. |
| Goober/skill change, worker restart, paused old run retry | Old run receives old pinned bytes, new run receives new bytes; no worker config-repo token. |
| Config seed stale or endpoint worker lacks generation transport | Prove usable pinned kit/generation or refuse explicitly; no current-disk fallback. |
| Merge authority revoked after reference admission | Historical bytes remain available; merge refused under current authority. |
| Local Foo -> shared cloud -> rollback | Preserve source definitions/history, one scheduler authority, claims reconciled before reactivation. |

These are implementation acceptance obligations, not tests claimed to pass in
this documentation change. The design PR runs the repository's design-status
and Markdown link checks.
