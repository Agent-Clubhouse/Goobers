# Design: Provider access layer — unified providers, connections and explicit credentials (DSL 3.0)

> Status: **draft** — long-term direction for PO review. Nothing here ships in v0.5.0.
> Near-term companion: [`ado-parity-dsl-2-0.md`](ado-parity-dsl-2-0.md) makes the
> existing DSL 2.0 work on Azure DevOps for v0.5.0; this document is where DSL 3.0
> goes after that.
> Completes: [ADR 0002](../adr/0002-provider-neutral-capability-namespaces.md)
> (provider-neutral capability names) and [`BL-033`](../requirements/backlog-providers.md)
> (a provider change requires no workflow change).
> Builds on: [`provider-contract-conformance.md`](provider-contract-conformance.md),
> [`multi-token-credentials.md`](multi-token-credentials.md) and
> [`trust-boundary-hardening.md`](trust-boundary-hardening.md).
> Tracking: #2061
> Verified: 47de1f0d6 (2026-09-25)

## 1. Summary

Goobers supports GitHub, Azure DevOps (ADO) and Gitea. The forge adapters are
provider-shaped, but three layers above them are GitHub-shaped:

- **Operation vocabulary.** The names a workflow declares are GitHub-spelled
  (`github:issues:write`, `github:pr:merge`). DSL 2.0 treats them as operations the
  routed provider performs; the near-term plan makes that rule explicit.
- **Credentials.** They are bound one per repository and keyed by opaque capability
  strings. They carry no notion of which provider or endpoint they belong to, and a
  gaggle cannot hold two credentials for one provider.
- **Providers.** One `Provider` interface bundles repository, pull-request and backlog
  operations, and a gaggle binds all of them to its project repository.

Real deployments need more than that. A large organisation may keep a backlog on
GitHub and code on ADO, or run a product whose client lives in ADO and service in
GitHub. It will want reviewer, lander and author identities kept apart, workload or
managed identity instead of PATs, and read access to repositories the gaggle never
writes to.

DSL 3.0 addresses this with five changes:

1. **Slots and bindings.** A workflow declares the named *slots* it needs (for
   example `code` and `work`) and the provider surfaces each slot requires. A gaggle
   defines named *bindings* to concrete targets on connections and wires slots to
   bindings. Nothing about the set of roles is fixed: names are custom, one binding can
   fill several slots, and a slot can take several bindings, so one gaggle can
   coordinate across providers.
2. **Neutral operations.** Every provider-dispatched operation is named
   `provider:<resource>:<verb>` (ADR 0002). Provider-specific names remain only for
   provider-specific semantics.
3. **Connections, identities and audience.** A connection is a provider endpoint with
   one or more named identities. Every credential carries the audience of the
   connection it came from, and injection enforces it.
4. **Explicit multiple credentials.** A gaggle, workflow or stage may use several
   read and write credentials, including several for the same provider. Every choice
   is explicit and resolvable at validation time, and ambiguity is an error.
5. **Metadata-driven registry.** Each operation declares its surface, class and required
   provider features. That drives admission, preflight, the landing-authority fence
   and the generated access matrix.

## 2. What exists today

The near-term plan (§1) and its appendices hold the full inventory. The points that
shape this design:

- **Three unconnected capability vocabularies.**
  - Authority capabilities (`internal/capability`, 26 names, 17 of them
    provider-named).
  - Provider features (`providers/capability.go`, e.g. `pr.merge`, already neutral).
  - Runner capabilities.

  Two hand-kept tables link the first two (`internal/providerstage/manifest.go` and
  `internal/instance/providercapability.go`), and a third opinion lives in the
  per-DSL policy-action tables.
- **Credential grants are opaque keys.** Every repository-token capability is backed
  by the gaggle repository's single secret by default
  (`internal/credentials/scoping.go`). Three different qualifier grammars have grown
  around it: `base@owner/name` for reference repositories, `base#harness:name` for
  per-harness grants (#5148), and a draft `base@site`.
- **Landing authority is a literal list.** `{github:pr:merge, ado:pr:complete}` is
  written into about eight places: the config-generation revocation fence, the worker
  plane, the harness, the HTTP API, the dispatcher, the manifest and the policy
  tables.
- **`Provider` is monolithic.** `RepoProvider + BacklogProvider + TriggerProvider` are
  mandatory together, so a backlog-only provider cannot be registered. Several
  concepts are GitHub's model in generic clothing:
  - a PR is addressed as an issue;
  - a numeric milestone is overloaded onto `WorkItem.Parent`;
  - identity is a single login string;
  - CI state is attached to commits.
- **Two competing credential designs are unimplemented.**
  - The local draft `explicit-stage-credential-bindings`: sites, `bindingName`, and
    `base@site`.
  - `multi-gaggle-validation`'s top-level `credentials:` list (#1794–#1800).

  Neither addresses provider-neutral names or audience.

## 3. Requirements

- **R1 — Operation naming (ADR 0002).** Provider-dispatched operations are
  `provider:<resource>:<verb>`. Provider-specific ones are `<provider>:<resource>:<verb>`.
  Names are not aliases.
- **R2 — Provider independence (BL-033).** Moving a binding to another provider changes
  bindings, never workflow or goober definitions.
- **R3 — Separable authority (pr-lifecycle §7, SEC-053).** Landing, review identity
  and trust decisions are separately grantable and revocable, and never implied by PR
  write.
- **R4 — Fail closed twice (ARCHITECTURE §5).** An undeclared operation is refused at
  validation, and receives no credential at runtime, on every provider.
- **R5 — Audience containment.** A credential is only exposed to clients and tools of
  the connection it was issued for.
- **R6 — Explicit multiplicity.** Several credentials are allowed. The choice between
  them is always written down and resolved deterministically before a run starts.
  Nothing is chosen at runtime by trial, and there is no fallback between identities.
- **R7 — Versioning.** New vocabulary arrives in DSL 3.x through manifest
  `sinceDSL`/`untilDSL` windows. DSL 2.0 keeps its meaning. `goobers fix` migrates,
  and pinned runs keep their compiled contract.
- **R8 — No phone-home (SEC-048).** Provider feature declarations stay static.
  Permission probes call only operator-configured endpoints.
- **R9 — Ledger is truth (BL-005).** Labels, tags and markers are projections. A
  provider may declare "no projection".
- **R10 — Intent-named operations (TBH-1 D1).** Operation names line up with the
  runner-owned adapter boundaries in `docs/stage-contract.md`.

## 4. Design

### D1. Connections, bindings and slots (fully composable)

The design has no fixed set of roles. Three layers compose instead.

**1. Connections** (instance level). A connection is a named provider endpoint with
its identities, declared once in `instance.yaml` for tiers 1–2 or the Manifest for
tier 3:

```yaml
connections:
  - name: ado-web
    provider: ado
    endpoint: https://dev.azure.com/example-org        # audience: ado @ dev.azure.com/example-org
    identities:
      author:  { auth: { kind: workload-identity, clientId: <author-app> } }
      lander:  { auth: { kind: workload-identity, clientId: <lander-app> } }
  - name: gh-service
    provider: github
    endpoint: https://github.com
    identities:
      author:  { auth: { kind: github-app, appId: 1, installationId: 2, privateKey: { store: kv/app-key } } }
      reader:  { token: { store: kv/readonly-pat } }
```

**2. Bindings** (gaggle level). A binding is a named, concrete target on a
connection. It exposes whichever provider *surfaces* the target supports: `repo`,
`pr`, `backlog` and `trigger`. These are the provider's declared features, not a role
list. An optional `surfaces` field narrows a binding for least privilege.

```yaml
bindings:
  client-code:  { connection: ado-web,    repository: example-project/client-app }
  service-code: { connection: gh-service, repository: example-org/service }
  work:         { connection: gh-service, repository: example-org/service, surfaces: [backlog] }
  boards:       { connection: ado-web,    backlog: { project: example-project, areaPath: "Client\\Web" } }
```

**3. Slots** (workflow level). A workflow declares the abstract slots it needs, like
function parameters. Each slot lists the surfaces it requires. Stages address slots,
never repositories.

```yaml
spec:
  slots:
    code:  { requires: [repo, pr] }
    work:  { requires: [backlog] }
    peers: { requires: [repo], many: true, optional: true }   # e.g. cross-repo context
```

**Wiring.** When a gaggle enables a workflow, it maps each slot to one binding, or to
several where the slot declares `many: true`. The same shipped workflow can be
enabled more than once with different wiring:

```yaml
workflows:
  implementation:
    slots: { code: client-code, work: work, peers: [service-code] }
  implementation-service:
    use: implementation
    slots: { code: service-code, work: work }
```

**Validation.**

- Every required slot is wired.
- Every wired binding exposes the surfaces its slot requires. This generalises
  CONF-6 to arbitrary slots.
- Every stage key `op@slot` names a slot whose declared surfaces include the
  operation's surface (D3).

**Composability.**

- Slot and binding names are user-defined.
- A binding can fill several slots, and a slot can take several bindings.
- Any provider can back any slot whose surfaces it supports.
- Cross-provider products (topology c) need no special concept.

**Defaults and sugar.**

- A gaggle with a single repository binds automatically to every slot whose surfaces
  that repository supports.
- The DSL 2.0 fields `project`, `backlog`, `additionalRepos` and `repos[]` lower to
  bindings named `project` and `backlog`, and to `peers`. Shipped workflows use
  matching default slot names, so existing instances keep their meaning with no edits.

**Underneath.** `Provider` splits into independently registrable surface providers:
`SourceProvider`, `ReviewProvider`, `BacklogProvider` and `TriggerProvider`. The
declared⇔implemented conformance rule applies per surface, and a backlog-only
provider becomes registrable. The claims ledger key gains the binding name, which
retires the draft's single-claim-site limit.

### D2. Neutral operation vocabulary

| Operation | Surface | Class | DSL 2.0 name(s) it replaces |
|---|---|---|---|
| `provider:backlog:read` | backlog | read | `github:issues:read` |
| `provider:backlog:write` | backlog | mutate | `github:issues:write`, `ado:work-items:write` |
| `provider:backlog:approve` | backlog | trust | `github:issues:approve` |
| `provider:backlog:plan` | backlog | mutate | `github:milestones:write` (milestone or iteration path) |
| `provider:pr:read` | review | read | `github:pr:read`, `ado:code:read` (PR inspection) |
| `provider:pr:write` | review | mutate | *(exists)* `github:pr:write`, `ado:pr:write`, `ado:pr:comment` |
| `provider:pr:review` | review | review-identity | `github:pr:review` |
| `provider:pr:status` | review | mutate | `ado:pr:status` |
| `provider:pr:land` | review | **landing** | `github:pr:merge`, `ado:pr:complete` |
| `provider:branch:delete` | source | mutate | `github:branch:delete` |
| `provider:ci:cancel` | review | mutate | *(exists)* |
| `repo:read`, `repo:push`, `contents:read` | source | read/mutate | *(already neutral)* |

- **Provider-specific names that stay.** They name semantics only one provider has:
  - `github:code-scanning:read` and `github:dependabot-alerts:read`;
  - a future `ado:policy:bypass`, a separate grant that no shipped workflow uses.
- **Retired from DSL 3.x.** The ADO names with no distinct semantics
  (`ado:code:read`, `ado:pr:comment`, `ado:pr:write`, `ado:work-items:write`) are
  removed from the vocabulary. In DSL 2.0 they stay valid, with the advisory the
  near-term plan adds.

### D3. Operation metadata

Each registry entry declares its surface, its class, the provider features its
operations need, whether it is neutral, and its DSL window:

```go
type Spec struct {
    Name     Capability
    Surface  Surface                 // repo | pr | backlog | trigger | none
    Class    Class                   // read | mutate | trust | review-identity | landing | service
    Features []providers.Capability  // provider features the operation needs
    Neutral  bool
    Provider providers.Kind          // provider-specific names only
    Since, Until dsl.Version
}
```

Consequences:

- **Landing is a class, not a list.** `Class == landing` replaces every literal pair.
  The revocation fence, the worker plane and the harness consult the class, so a
  future provider cannot bypass them.
- **One source of truth.** Commands declare operations. Manifest requirements, CONF-6
  feature preflight and policy-action requirements are all derived from those
  declarations, so they cannot disagree.
- **Class drives policy.** TBH migration order, agentic-mutation audits and default
  identity selection (D4) key on `Class`.

### D4. Credentials: audience, identities, explicit selection

**Audience.** Every materialised credential carries `(provider, endpoint)` from its
connection. Consumers declare what they accept:

- a provider client accepts only its connection's audience;
- a harness adapter maps each environment variable to one audience;
- an MCP server's credential reference names a connection.

A mismatch is refused at admission, with a diagnostic. The operator never types an
audience, so it cannot be mis-set.

**Identities.** A connection has one or more named identities. The default selection
is by operation class:

| Class | Default identity |
|---|---|
| read | `reader` if declared, else `default` |
| mutate | `author` if declared, else `default` |
| review-identity | `reviewer` if declared, else the mutate default, **with a warning** |
| landing | `lander` if declared, else the mutate default, **with a warning** |
| trust | `approver` if declared, else the mutate default, **with a warning** |

**Separation is recommended, and the fallback is visible (PO decision).** When a
landing, review or trust operation falls back to the author identity, validation
emits a warning naming the operation, the binding and the identity it fell back to.
Separate identities match ADO's split permissions ("Contribute to pull requests",
"Contribute", "Bypass") and its option to discount the creator's vote. A gaggle can
silence the warning by naming the shared identity explicitly (`~author`), which
records the choice.

**Explicit multiplicity.** Stage declarations name exactly which slot, and
optionally which identity, an operation uses:

```yaml
capabilities:
  - provider:backlog:write                 # the only slot exposing backlog
  - repo:read@peers                        # every binding wired to peers
  - repo:push@code
  - provider:pr:write@code
  - provider:pr:land@code~lander           # explicit identity
  - { op: provider:pr:review, slot: code, identity: reviewer }   # object form
```

- **One key grammar, two spellings (PO decision).**
  - The string form is `<operation>[@<slot>][~<identity>][#harness:<name>]`.
  - The object form is `{op, slot, identity, harness}`.
  - Both lower to one internal key, parsed once in `internal/capability`. The JSON
    schemas validate the string with a pattern and the object with a schema.
  - The existing `base@owner/name` and `#harness:` keys lower into it.
- **Resolution.** For each declared key: slot → wired binding(s) → connection →
  identity → credential source. The most specific choice wins, in this order:
  1. the stage key;
  2. the workflow's slot wiring in the gaggle;
  3. the binding's default identity;
  4. the connection's class default.
- **Ambiguity is a validation error.** Two sources at the same specificity for the
  same (operation, audience, scope) fail validation. An unqualified operation that
  matches several slots also fails, and the error lists the qualifiers to choose
  from.
- **No runtime fallback** between identities or credentials.
- **Several credentials in one stage.** This is allowed when each is qualified. Each
  credential is delivered under a distinct, audience-checked variable or client. An
  agentic stage gets at most one credential per audience per harness variable. When
  it needs more, that is expressed as separate MCP servers or deterministic stages.
- **Delivery.** Every source resolves daemon-side. PATs come from stores, and the
  daemon mints GitHub App, Entra (azure-cli, workload identity, managed identity) and
  future OIDC tokens. Credentials reach stages through the injector for local stages
  and the credential plane for pods. Stages never read `instance.yaml` credentials,
  and nothing needs `envPassthrough`.
- **Audit.** The journal records the slot, binding, connection and identity name used by
  each stage. It never records the secret.

**Converging the two drafts.** From the `explicit-stage-credential-bindings` draft,
this model keeps named sites (now bindings), `bindingName` (now connections) and
the default-deny rule for agentic writes to non-project repositories. From
`multi-gaggle-validation`, it keeps first-class credential entries (now identities)
and their per-source kinds. Both drafts' capability names give way to D2. #1794–#1800
are re-scoped against this model, and the draft is superseded (PO decision).

### D5. Access modelled in three layers

| Layer | Owner | Mechanism |
|---|---|---|
| Goobers operation | workflow and goober | admission (D3) and audience-checked non-injection (D4) |
| Token scope | credential source | per-provider requirement table (GitHub fine-grained permission, Gitea scope, ADO `vso.*` scope) |
| Object permission and licence | provider admin | per-provider table, e.g. ADO Contribute, Contribute to pull requests, Bypass, Basic access, area-path permissions |

- **Generated access matrix.** `make docs` generates `docs/provider-access-matrix.md`
  from per-operation, per-provider requirement rows. An operation with no row for a
  blessed provider fails the conformance gate unless it declares `unsupported`.
- **Validation probes.** `validate --check-repos` checks layers 2–3 for every bound
  identity with read-only calls to configured endpoints. It reports the missing
  permission by name, and warns when a landing or approval identity also holds a
  bypass permission.
- **Throttling models.** Throttling is reported per provider: GitHub's hourly budget,
  ADO's per-identity TSTU window with pre-429 delays, and Gitea's instance
  configuration. The quota ledger is keyed by identity, not by provider.

### D6. Neutral concepts in the provider contract

- **Markers.** Stages write intent (`mark needs-remediation`). The provider projects
  it as a label, tag or nothing, declared per surface. PR markers never go through
  work-item APIs, and gating never trusts a projection anyone can edit.
- **State categories.** Work items expose Proposed, InProgress, Resolved, Completed
  and Removed. Providers map their native states to these.
- **Planning bucket.** Covers milestones and iteration paths through
  `provider:backlog:plan`. `Parent` means hierarchy only.
- **Identity triple.** `{id, login, display}`. Every "is this me" check uses `id`.
- **Merge readiness.** GitHub required checks, ADO policy evaluations and Gitea
  combined status, with a distinct "waiting on a human" state.
- **Landing.** Direct, queue or auto-complete, asynchronous and head-pinned where the
  provider supports it. It never bypasses policy.
- **Relations.** `blocks`, `blockedBy` and `parent`, with the related item's state
  category.

### D7. Versioning and migration

- **Where the new names land.** The D2 names and the D4 qualifier grammar land in the
  next DSL minor, 3.x, through manifest windows and per-DSL policy tables. DSL 2.0
  views keep today's names and the rebinding rule.
- **`goobers fix`.** It rewrites capabilities, lowers `project`/`backlog`/`repos[]`/
  `credentials:` into `connections` and `bindings` when a workflow's `dslVersion` is
  bumped, and prints the identity decisions it could not infer. For example, a
  landing identity must be chosen explicitly.
- **What moves together, per operation family:**
  - registry entries;
  - JSON schemas;
  - `credentialedCapabilities`;
  - daemon-identity defaults;
  - harness environment maps;
  - `GOOBERS_CRED_*` names;
  - shipped workflows.
- **Pinned runs** are unaffected.

### D8. Conformance

- **Compile matrix.** Every shipped workflow is compiled × every blessed provider ×
  representative topologies: single provider, mixed backlog and source, and
  multi-source.
- **Live legs per provider.** These include identity-separation cases: a reviewer
  identity distinct from the author, and a lander identity without bypass.
- **Access matrix completeness.** Checked by the D5 generated-matrix rule.

## 5. Phasing

The near-term plan's items are prerequisites. It lands daemon-side credential minting
for ADO, the compile-matrix gate, live ADO legs, audience containment for repository
credentials, and the ADO PR and backlog fixes.

| Phase | Scope | Depends on |
|---|---|---|
| L1 | D3 operation metadata; landing as a class; derive manifest, feature and policy tables | near-term plan |
| L2 | D4 audience on every credential; connection and identity model in config (sugar-compatible with DSL 2.0 config) | L1 |
| L3 | D2 vocabulary and the D4 qualifier grammar in DSL 3.x; `goobers fix`; generated access matrix (D5) | L1, L2 |
| L4 | D1 slots, bindings and wiring; surface provider split; multi-binding slots; claims keyed by binding | L2, L3 |
| L5 | D6 neutral concepts | L4 |

Each phase becomes a set of single-PR issues linked to this document once it is
approved.

## 6. Alternatives considered

- **Aliases for GitHub and ADO names.** Rejected by ADR 0002: two spellings for one
  authority make admission ambiguous.
- **GitHub names as the permanent neutral vocabulary.** Keeps non-GitHub authors
  reading GitHub terms, and leaves landing tied to a provider name. The DSL 2.0
  rebinding rule is a bridge, not the destination.
- **Per-provider workflow variants.** Violate BL-033 and multiply the shipped surface.
- **Runtime credential fallback** (try identity A, then B). Violates R6. Behaviour
  would depend on which call failed first, and the audit trail loses meaning.

## 7. PO decisions (2026-09-25)

1. **Composable model.** Adopt connections, bindings and workflow-declared slots
   (D1) as the DSL 3.0 direction, not a fixed set of roles. Names are custom, and
   slots and bindings compose many-to-many.
2. **Separated identities.** Landing, review and trust fall back to the author
   identity **with a validation warning**. Naming the shared identity explicitly
   silences it (D4).
3. **Older designs.** Once this design is approved, the
   `explicit-stage-credential-bindings` draft is superseded, and #1794–#1800 are
   re-scoped against D1 and D4, or closed where obsolete.
4. **Qualifier grammar.** Both the string form `<op>[@<slot>][~<identity>][#harness:<name>]`
   and the object form `{op, slot, identity, harness}` are accepted, and both lower to
   one key (D4).
