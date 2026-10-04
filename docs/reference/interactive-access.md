# Interactive human access

Restart admission uses a single policy lease when checking repository and backlog
sources. The two sources resolve independently through their named interactive
credentials; repository access never supplies a missing backlog identity. The
callback may verify source state and accept a restart while policy is stable.
Asynchronous execution needs its own interactive credential binding and must not
retain these callback-scoped credentials. This prerequisite does not itself
enable the Restart action.

Interactive access is opt-in per gaggle. Existing monitoring retains its instance
role behavior. New interactive sessions, provider-backed browsing and writes
require both a verified instance role and an explicit gaggle grant. An instance
administrator has no implicit gaggle grant. Anonymous loopback requests cannot
use the interactive permission route.

The current implementation provides policy authorization, named credential
selection, `GET /api/v1/gaggles/{gaggle}/interactive-capabilities`, and a shared
portal surface for local run gate decisions and saved guidance. Sessions, live
agent steering and source editing remain unavailable. Stage restart requires
an installed interactive execution adapter; its daemon capability defaults off. The permission response separates `authorized`,
`credentialConfigured` and `available`; `run.intervene` is implemented. `run.restartStage` is advertised only after the
daemon installs its execution adapter. The run service checks target support.

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
  actions: [run.intervene, run.restartStage, backlog.read, backlog.edit, repository.read, source.proposeChange]
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
- `POST /api/v1/runs/{run}/interactive-commands`: `approve`, `override`, `deny`,
  `guidance`, or `restart`, with `Idempotency-Key`, a stage and `expectedSubjectSequence` from
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
A saved note has not been delivered to an agent and does not resume work. Restart operations explicitly select retained note IDs as context. The
current surface accepts at most 64 KiB UTF-8 guidance or 4096 bytes of rationale,
retains at most 100 shared notes per run, and refuses a retained journal larger
than 32 MiB on this initial event-scan path. The journal remains the authoritative
history; monitoring and existing journal interfaces stay available.

Human content passes through the shared credential registry and pattern scrubber
before fingerprinting and persistence. Provider identities are unnecessary for saving guidance and recording gate
decisions. Restart admission and execution require configured interactive identities. Repository changes continue to require the
configured source identity and a pull request.

## Restart a settled stage

With the production adapter installed, an operator with `run.restartStage` can
select one to 16 saved notes and a rationale in the run panel. The API command
uses `kind: restart`, `guidanceIds`, `rationale`, the affected `stage`, and its
observed terminal `expectedSubjectSequence`.

The first supported target is an agentic task or reviewer in a settled failed
or escalated local DSL 3.1 run. The restart creates a distinct linked execution.
The original journal is immutable. Upstream scalar context and selected retained
artifact pointers are restored; input and guidance snapshots are bounded and
scrubbed. The affected stage and its returning review gates receive a fresh
retry/repass allowance. Other counters remain in force. Recovery consumes the
same epoch allowance and guidance snapshot; it cannot mint another allowance.
The original run duration limit is retained, and prior usage and history remain
in the linked source journals.

`started` means a durable execution was accepted; it does not claim the agent
already read the guidance. `continuationRunId` links to that execution. Retrying
an uncertain response with the same key and payload returns the same epoch.
An existing active or terminal epoch does not repeat provider admission checks;
an unowned unfinished epoch is revalidated before recovery. A changed payload
with the same key is rejected. Ordinary automation runners refuse a human
restart marker, including after daemon recovery.

Paused same-run fresh allowances, parallel branch/fan-in restoration, Temporal,
and settled generated-child continuations require additional runtime support.
They are unavailable in this adapter. In particular, a child's sealed terminal
result cannot be replaced without an accepted parent-observation and workspace
mapping. Existing paused gate decisions and saved guidance remain available.

### Admission and source custody

The restart admission adapter keeps the original authenticated issuer, subject,
roles and group claims in a bounded trusted input. These are separate fields;
the display actor string is never parsed into an identity. Exact retries retain
that snapshot while checking current gaggle authorization. No provider secret
or bearer token is stored with it. A receipt replay does not resolve credentials
again or contact the forge.

New execution checks the applied workflow and gaggle, the archived source
repository and backlog locations, current open claims, and the exact published
branch head. Code and backlog use independently selected interactive credentials,
including mixed providers. ADO branch lookup matches the full ref name; a prefix
match is insufficient. The provider checks and bounded acceptance share one
policy lease, with a 30-second admission deadline. Subsequent execution must
independently enforce the current interactive identity.

The initial admission path refuses held workspaces, retained recovery snapshots,
unattributed recovery state, shared claims without an explicit lease transfer,
and PR claims whose repository cannot be identified unambiguously. It preserves
that work for explicit restoration or adoption. It also refuses a source branch
that moved after the recorded commit. These checks do not reset a workspace or
publish a provider mutation. The daemon installs the dedicated interactive execution builder for the
supported local backend described below. Unsupported stages are refused before
acceptance.

### Local restart execution identity

The local restart adapter supports DSL 3.1 workflows whose execution consists
of Claude Code or Codex agents/reviewers using explicitly configured model API
keys, human/automated gates, and native GitHub/ADO CI polling. Agentic sandboxing
must be enforced, and the host must support process-termination verification. Shell stages, instance-aware provider CLI stages, remote
placement, custom harness launchers, external MCP servers, child delegation,
experiments, and merge/delete/config-repository capabilities are refused before
acceptance. The ordinary monitoring and automation paths retain their behavior.

Repository and backlog access resolve their independently named interactive
credentials; the model key remains separate. Agent tools receive capability-
specific credential variables rather than a shared ambient GitHub identity.
Native repository reads, Git fetches, and CI polling use the same human-selected
sources. Automatic failure/escalation/provider cleanup hooks are disabled on
this driver because those hooks otherwise use automation credentials.

The runtime uses a clean home and environment and denies known host Git, SSH,
forge, and cloud authentication paths. Existing refusal of configured credential
files remains in force. This operates within the existing local sandbox trust
model; it is not a security boundary against arbitrary same-user host credential
extraction or operating-system keychain access. Use an isolated worker for that
stronger boundary.

The exact configuration archive stays leased through execution. A changed
gaggle policy cancels affected human executions and waits for them to join
before publication. Publication is refused if the bounded drain cannot prove
that execution returned; cancellation alone is not treated as revocation
completion. Recovery must rebuild this dedicated human driver from the durable
verified principal and current policy, and never select an automation runner.
