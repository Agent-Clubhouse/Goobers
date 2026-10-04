# Interactive human access

Interactive access is opt-in per gaggle. Existing monitoring retains its instance
role behavior. New interactive sessions, provider-backed browsing and writes
require both a verified instance role and an explicit gaggle grant. An instance
administrator has no implicit gaggle grant. Anonymous loopback requests cannot
use the interactive permission route.

The current implementation provides policy authorization, named credential
selection, `GET /api/v1/gaggles/{gaggle}/interactive-capabilities`, and a shared
portal surface for local run gate decisions and saved guidance. Sessions, live
agent steering, source editing and fresh-allowance stage restarts remain
unavailable. The permission response separates `authorized`,
`credentialConfigured` and `available`; `run.intervene` is implemented, with
run-specific availability checked by the interactive run service.

## Human membership

```yaml
# In instance.yaml api.auth.oidc, alongside issuer/audience/roles:
groupsClaim: groups
```

The group claim defaults to `groups` and is independent of instance role mapping.
Only a verified JWT supplies group membership. Missing, malformed or oversized
group claims grant no groups, while preserving authenticated subject and
instance roles for existing monitoring. Grant identities use exact issuer and
subject or group, never display names.

```yaml
# Gaggle spec:
interactiveAccess:
  humans:
    viewers:
      - issuer: https://identity.example
        group: factory-readers
    operators:
      - issuer: https://identity.example
        subject: alice
  actions: [run.intervene, backlog.read, backlog.edit, repository.read, source.proposeChange]
  credentials:
    backlog: human-issues
    repositories:
      - repository: {provider: github, owner: acme, name: web}
        credentialRef: human-code
  sourceWrites:
    mode: pull-request
```

A viewer also needs the instance `view` role. An operator also needs `operate`
(or `admin`). Operators have viewer access. Actions are an explicit allowlist;
empty actions enable only permission inspection. Omitted `sourceWrites` still
enforces pull-request publication. Direct repository publication is unsupported.

## Explicit execution identities

```yaml
# instance.yaml; these are independent from repos and automation credentials.
interactiveCredentials:
  - name: human-issues
    provider: github
    owner: acme
    repository: issues
    token: {env: HUMAN_ISSUES_TOKEN}
  - name: human-code
    provider: github
    owner: acme
    repository: web
    token: {store: vault/human-code-token}
  - name: human-ado-backlog
    provider: ado
    owner: organization
    project: Work Project
    token: {env: HUMAN_ADO_TOKEN}
```

These named sources reuse existing token references and GitHub App / ADO
identity authentication. They do not introduce a separate secret store. ADO
project-only backlog sources omit `repository`; repository access requires an
exact repository. GitHub requires `repository` and forbids `project`.

The backlog and every repository select their own named source. There is no
fallback to the automation identity, first repository, `connectionRef`, or a
credential from another target. Repository identities must belong to the
configured gaggle. The existing supported provider topology remains unchanged:
ADO code may use a GitHub backlog; an ADO backlog derives its organization from
an ADO code project. A GitHub owner is never inferred to be an ADO organization.

Do not add human credential environment variables to `runner.envPassthrough`.
The configuration rejects that overlap. Resolved secrets are registered with the
daemon scrubber before use and never appear in the permissions response.

Applied policy reload is fenced against each bounded provider effect. New
operations use the new grants once the scheduler catalog is published; a failed
catalog publication retains the previously applied policy. Credential selection
is rechecked for every operation. Instance credential-source changes require
restarting the daemon, like other instance configuration changes.

## Shared run operations

The run page shows a **Human operations** panel independently of ordinary
monitoring. Its two human-only routes are:

- `GET /api/v1/runs/{run}/interactive`: current actions and shared saved guidance.
- `POST /api/v1/runs/{run}/interactive-commands`: `approve`, `override`, `deny`, or
  `guidance`, with `Idempotency-Key`, a stage and `expectedSubjectSequence` from
  the read. The body cannot supply an actor, gaggle or credential source.

An operator needs the explicit gaggle `run.intervene` action and the instance
`operate` role. Human-gate approver rules still apply. Approval and override use
the existing pinned local runner continuation; denial records a reviewed
escalation and leaves the run terminal. This does not create the new restart
allowance epoch described in the design. Temporal/engine runs are explicitly
unsupported on this new surface; their existing interfaces are unchanged.

Decisions bind to an unresolved human gate pause or a terminal generation. A
stale generation is refused before execution. The actor's issuer and subject,
scoped idempotency key, action and rationale are retained in the journal. The
response reports `applied` only after decision evidence exists, `failed` after a
durable pre-application failure receipt, and `pending` when the request budget
ends before the outcome is known. Retry an uncertain operation using the same
key and payload. The portal preserves both and never automatically retries a
mutation. A failure in subsequent resumed work remains that work's failure.

Guidance is durably saved as an operator-message record with delivery mode
`shared-guidance`, purpose `stage-restart-guidance` and target
`stage:<name>@<observed-sequence>`. Authorized gaggle viewers share these records.
A saved note has not been delivered to an agent and does not resume work. Future
restart operations must explicitly select retained note IDs as context. The
current surface accepts at most 64 KiB UTF-8 guidance or 4096 bytes of rationale,
retains at most 100 shared notes per run, and refuses a retained journal larger
than 32 MiB on this initial event-scan path. The journal remains the authoritative
history; monitoring and existing journal interfaces stay available.

Human content passes through the shared credential registry and pattern scrubber
before fingerprinting and persistence. Provider identities are unnecessary for
these journal/runner operations. Repository changes continue to require the
configured source identity and a pull request.
