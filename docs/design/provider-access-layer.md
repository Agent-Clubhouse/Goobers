# Design: Provider access layer — unified providers, connections and explicit credentials (DSL 3.0)

> Status: **draft** — long-term direction for PO review. Nothing here ships in v0.5.0.
> Near-term companion: `docs/design/ado-parity-dsl-2-0.md` ([#5665](https://github.com/Agent-Clubhouse/Goobers/pull/5665)) makes the
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

1. **Roles.** A gaggle binds *source*, *review*, *backlog* and *trigger* roles, each
   to a named connection. More than one source or review connection is allowed, so one
   gaggle can coordinate across providers.
2. **Neutral operations.** Every provider-dispatched operation is named
   `provider:<resource>:<verb>` (ADR 0002). Provider-specific names remain only for
   provider-specific semantics.
3. **Connections, identities and audience.** A connection is a provider endpoint with
   one or more named identities. Every credential carries the audience of the
   connection it came from, and injection enforces it.
4. **Explicit multiple credentials.** A gaggle, workflow or stage may use several
   read and write credentials, including several for the same provider. Every choice
   is explicit and resolvable at validation time, and ambiguity is an error.
5. **Metadata-driven registry.** Each operation declares its role, class and required
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
- **R2 — Provider independence (BL-033).** Moving a role to another provider changes
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

### D1. Roles and connections

**Connection.** A connection is a named provider endpoint with its identities. It is
declared once, in `instance.yaml` for tiers 1–2 or the Manifest for tier 3:

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

**Role binding.** The gaggle binds roles to connections and repositories:

```yaml
spec:
  roles:
    source:
      - name: client            # an ADO repository
        connection: ado-web
        repository: example-project/client-app
      - name: service           # a GitHub repository
        connection: gh-service
        repository: example-org/service
    review:  [client, service]  # PRs are opened where the code lives
    backlog:
      connection: gh-service
      repository: example-org/service   # GitHub Issues as the backlog
    trigger: { mode: poll }
```

- **Defaults and sugar.** `review` defaults to the `source` entries. A gaggle with a
  single source entry needs no names. The DSL 2.0 fields (`project`, `backlog`,
  `additionalRepos`, `repos[]`) remain as sugar that lowers to exactly this shape, so
  existing instances keep their meaning.
- **Provider interface split.** `Provider` becomes independently registrable
  `SourceProvider`, `ReviewProvider`, `BacklogProvider` and `TriggerProvider`.
  Provider features partition by prefix (`repo.*`, `pr.*`, `backlog.*`, `trigger.*`),
  and the declared⇔implemented conformance rule applies per role. A backlog-only
  provider becomes registrable.
- **Topology (c).** A product that spans ADO and GitHub is one gaggle with two
  `source` entries. A stage addresses a specific one with a qualifier (D4). The claims
  ledger key gains a repository dimension (`ClaimKey` + role entry), which also
  retires the draft's single-claim-site limit.

### D2. Neutral operation vocabulary

| Operation | Role | Class | DSL 2.0 name(s) it replaces |
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

Each registry entry declares its role, its class, the provider features its
operations need, whether it is neutral, and its DSL window:

```go
type Spec struct {
    Name     Capability
    Role     Role                    // source | review | backlog | trigger | none
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
| review-identity | `reviewer`, **required** (no default) |
| landing | `lander`, **required** (no default) |
| trust | `approver`, **required** (no default) |

The three separated classes must be bound explicitly. They never share the author's
secret implicitly. On ADO this matches its split permissions: "Contribute to pull
requests", "Contribute" and "Bypass". It also matches ADO's rule of discounting the
creator's vote.

**Explicit multiplicity.** Workflow and stage declarations can name exactly which
binding an operation uses. A qualifier addresses the role entry, and optionally an
identity:

```yaml
capabilities:
  - provider:backlog:write                 # the backlog role, class default identity
  - repo:read@service                      # read the GitHub service repo
  - repo:push@client                       # push to the ADO client repo
  - provider:pr:write@client
  - provider:pr:write@service
  - provider:pr:land@client~lander         # explicit identity
```

- **One key grammar.** `<operation>[@<role-entry>][~<identity>][#harness:<name>]`,
  parsed once in `internal/capability`. The JSON schemas validate it with a pattern
  instead of enums. The existing `base@owner/name` keys and `#harness:` keys lower
  into it.
- **Resolution.** For each declared key: role entry → connection → identity →
  credential source. The most specific binding wins, in this order:
  1. stage key;
  2. workflow default;
  3. gaggle role default;
  4. connection class default.
- **Ambiguity is a validation error.** Two sources at the same specificity for the
  same (operation, audience, scope) fail validation. An unqualified operation in a
  gaggle with several matching role entries also fails, and the error lists the
  qualifiers to choose from.
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
- **Audit.** The journal records the role entry, connection and identity name used by
  each stage. It never records the secret.

**Converging the two drafts.** From the `explicit-stage-credential-bindings` draft,
this model keeps named sites (now role entries), `bindingName` (now connections) and
the default-deny rule for agentic writes to non-project repositories. From
`multi-gaggle-validation`, it keeps first-class credential entries (now identities)
and their per-source kinds. Both drafts' capability names give way to D2. #1794–#1800
are re-scoped against this model.

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
  it as a label, tag or nothing, declared per role. PR markers never go through
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
  `credentials:` into `connections` and `roles` when a workflow's `dslVersion` is
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
| L4 | D1 roles and provider interface split; multi-source gaggles; claims keyed by role entry | L2, L3 |
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

## 7. Decisions needed from the PO

1. **Adopt roles and connections (D1, D4) as the DSL 3.0 direction.** This includes
   the multi-source gaggle for cross-provider products. *Recommendation: yes.*
2. **Required explicit identities for landing, review and trust (D4).**
   *Recommendation: yes.* A gaggle that wants one identity for everything says so
   explicitly with the same identity name.
3. **Re-scope #1794–#1800 and the explicit-stage-credential-bindings draft against
   D4.** *Recommendation: yes.* Close the draft as superseded once this is approved.
4. **Qualifier grammar (D4).** Approve `@<role-entry>` and `~<identity>`, or choose a
   different spelling before any schema work starts.
