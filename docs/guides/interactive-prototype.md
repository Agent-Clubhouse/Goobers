# Interactive sessions and delegated access prototype

This is the runnable checkpoint on `experimental/interactive`, based on main
`4e6c7d1e30c76718a56de2b9ba940c17ef9333e5`. It combines two optional portal
experiences with durable sessions, private working copies, bounded execution,
and mediated delegated-access plumbing. It is an experiment, not a cloud
deployment or a production authentication system. Ordinary declarative
workflows keep their existing execution path.

## Launch locally

Use the Go version pinned in `go.mod`, Node.js 24, npm, and Git. On macOS,
native execution needs permission to inspect its owned process tree and invoke
the existing nested `network: none` sandbox. An outer restricted sandbox can
prevent this; cleanup uncertainty is surfaced rather than treated as success.

From the repository root:

```sh
npm ci --prefix portal
npm run build --prefix portal
mkdir -p .tools
go build -o .tools/goobers ./cmd/goobers
go build -o .tools/interactive-prototype ./cmd/interactive-prototype
.tools/interactive-prototype \
  --state-dir /tmp/goobers-interactive-demo \
  --executor local-tools
```

Open <http://127.0.0.1:8097/#/interactive>. Keep the state directory outside
every Git checkout and use a fresh private directory for a clean demonstration.
Stop the server with Ctrl-C and wait for confirmed cleanup. Reuse the state
directory to inspect durable history. After restart, controlled connections
require explicit reauthentication; the server does not invent a credential
cache. Revoked fixture connections cannot be revived; use a fresh state
directory for another independent demo.

The app shows **Gaggle Helper** and **Cloud Workbench**. “Cloud” describes the
intended experience; this checkpoint uses a local worker. The local-persona
selector offers Mason and Alex so isolation can be exercised with two browser
contexts. These are deliberately selectable test identities, not verified
Entra users. The server only binds loopback, checks Host/Origin and cross-site
requests, and takes ownership from its configured persona adapter rather than
an arbitrary owner field in a command.

## What actually executes

| Component | Checkpoint behavior |
| --- | --- |
| Portal | React UI against the real Go HTTP/SSE backend; no browser API mocks in acceptance tests |
| Local tools executor | Explicit deterministic intent driver; actual source reads, edits, Git review, Node test subprocesses, steering and cancellation |
| Native Helper controls | Actual Goobers config validation/transactions, scheduler admission receipts, workflow runner, process custody and journals |
| Native child review | A generated bounded test-only declarative workflow in the child's independent snapshot, with actual native run receipts |
| Codex executor | Existing `harness.CodexAdapter` integration; optional trusted local CLI login; model-driven smoke requires separate opt-in |
| ADO/App Insights | Controlled local typed providers and observations, including a durable local mutation ledger; no Microsoft API calls |
| Sign-in | Controlled local personas; no live authentication or newly created credentials/grants |

The default local executor is **not a language model**. It supports
`investigate`, `fix` (the supplied invoice bug), `test`, `review`,
`read <relative path>`, and `wait`. It explains its limitations in the
conversation. The default source is a disposable, newly authored invoice
sample with a real failing test: excessive discounts produce a negative total.
No backlog item or authored workflow is required before opening a Workbench
session and editing it.

The optional model mode is selected explicitly:

```sh
.tools/interactive-prototype \
  --state-dir /tmp/goobers-interactive-codex \
  --executor codex-local \
  --trusted-local-model-login
```

This uses an existing local Codex CLI login and may transmit the chosen
workspace and controlled provider observations to that model service. It is a
trusted single-user workstation boundary, not per-user cloud model identity.
It does not supply Microsoft tokens. The checkpoint's attempted model smoke
was denied before execution by automatic approval review pending explicit
approval of the sample payload and model destination. Until that opt-in is
approved and the test passes, do not describe this checkpoint as qualified
model-driven execution.

## Demonstration journeys

1. Open a Workbench session. Send `investigate`. Observe the fixture ADO bug
   and App Insights query, actual source, streamed tool output, and the failing
   Node assertion. Send `fix`, then `test`. Review the cumulative changes.
   Cumulative review survives private Git commits and clears after reversion.
2. Send `wait` and steer it. The UI distinguishes accepted steering from the
   executor's acknowledgment at a tool boundary. Disconnect and reconnect the
   stream, or reload the page; replay preserves ordered history. A lost command
   response can be retried with its original stable identity without dispatching
   another effect.
3. At an idle checkpoint, propose a child review, inspect its isolated scope,
   approve it, and mark the result reviewed. Local mode creates a bounded
   native test workflow against an independent copy of the current parent
   workspace. It neither adopts a patch nor performs an open-ended semantic
   review. The parent cannot edit during a running child snapshot/review.
4. Open a Helper session and send `gaggle status` to initialize its private
   workspace under a worker lease. Inspect real isolated gaggle config, workflow state
   and run journals; its backlog context is a labeled fixture. Ask to
   `design workflow`, review/edit the allowed YAML in Actions, apply it, then
   submit a run. The first run fails on the real invoice assertion. Fix the
   same session workspace and rerun; the native workflow now sees that edit.
   Try `rename gaggle`, enabling/disabling a workflow, and stopping a run.
   Exact reviewed proposals are required before configuration or run effects.
5. In a second browser context select Alex. Open an independent session and
   compare workspaces, changes and history. Owner-scoped API lookups and
   commands refuse cross-owner access. Simultaneous sessions have separate
   leases and working copies; they do not share one mutable agent runtime.
6. Expire a controlled connection, observe an auth wait, reauthenticate and
   resume. Resume reuses any provider receipt already completed in that turn.
   Demonstrate silent refresh separately. Revoke during execution: authority
   is fenced first, cancellation follows, and stopped sessions cannot resume.
7. Explicitly stop a running turn or Helper workflow. The UI distinguishes
   stop requested, stopping, stopped, and custody/recovery pending. Closing the
   browser does not stop the worker or claim cleanup.

## Session and authority decisions

The session coordinator stores ordered events, command receipts, messages,
leases, children and cumulative changes in private atomic files. Stable
command IDs bind to exact admitted payloads. Rejected stale sequences may be
refreshed only after an explicit pre-dispatch rejection.
An unknown transport outcome retains its original identity and exact last
payload for reconciliation. The portal retries an explicit stale-sequence
rejection at most four times with the same command identity and a refreshed
sequence; it refuses automatic retry after a changed fence or steering target.
SSE replay uses sequence cursors. One process holds the coordinator store lock.

Workers heartbeat leases and carry fences through provider admission. A normal
completed turn releases capacity and leaves an idle conversation. A lost lease
or interrupted writer requires recovery. The executor keeps a writer marker
outside the model-writable workspace; a crashed process cannot establish its
own cleanup by removing a marker. Recovery never takes over an unconfirmed
writer. Operator isolation/reconciliation is needed after an actual crash with
unknown custody; there is no unsafe “force resume” button.

Default capacity is four concurrent interactive executions and, separately,
four native workflow workers, with two per owner in each pool. Children count
against these limits. The total can therefore reach eight across both pools.
Native admission is cross-process on one supported host; the session store has
one coordinator. There is no distributed queue, Kubernetes worker provisioner,
multi-host failover or autoscaling qualification in this checkpoint.

Sessions have explicit owner/membership/actor fields but enforce owner-only
access. Sharing a live session, collaborator roles, simultaneous human edits,
and collaborative approval semantics remain future work.

The host issues immutable authority revisions with exact resources/actions,
an original two-hour deadline, shared 80-operation budget, three cumulative
children, and depth one. Child grants attenuate the parent; stop, revocation
and parent supersession fence them. Rebinding a recovered execution preserves
the original deadline and consumed budget. Consent/reauthentication never
authorizes a pending mutation by itself. Unknown provider outcomes are
reconciled from receipts and never blindly retried.

These are reversible prototype limits, not accepted product defaults. See
[the broker contract](../../internal/interactiveauth/README.md) and
[native Helper operations](../../internal/interactiveops/README.md) for exact
operations, effects and failure semantics.

## Customization and boundaries

Pass `--surfaces /absolute/path/surfaces.json` to configure the two experiences:

```json
[
  {"id":"helper","name":"Team Helper","mode":"helper","description":"Operate the isolated gaggle","enabled":true,"maxChildren":2,"instructions":"Ground work in configured workflows and observed runs."},
  {"id":"workbench","name":"Engineering Workspace","mode":"workbench","description":"Investigate, edit and test","enabled":true,"maxChildren":3,"instructions":"Preserve changes for review."}
]
```

Set either `enabled` to false to disable that experience. Existing history
remains inspectable; disabling a surface fences new control. `--local-actors`,
`--lease-duration` and `--max-active` configure the local demonstration.
`--source` accepts only a trusted host-selected source directory, never a
browser-supplied path. Snapshots reject links/special files, exclude common
secret filenames and hidden files, and impose size limits. This is not a
general secret scanner or a sandbox for arbitrary untrusted repositories.
Native child code still executes as the host user inside the existing native
workflow boundary. Do not expose this server remotely.

Helper chat is a bounded intent parser. Native config permits deterministic
test-only workflows with restricted arguments, deadlines and `network: none`.
Dynamic children use this bounded ordinary-workflow path; they do not yet
integrate the production `childworkflow` journal lineage/admission model or
allow arbitrary generated workflow capabilities. The optional Codex path can
investigate/edit arbitrary trusted sample code, but its native tool semantics
and live model behavior still need the blocked smoke qualification.

Mediated ADO/Azure calls receive a reference to authority, never a reusable
provider token. Native Azure CLI, Git helpers, generic HTTP clients and
token-consuming MCP servers cannot transparently inherit this authority.
Supporting those tools requires an explicit future adapter or a separately
approved weaker credential-custody boundary. There is no application-identity
fallback.

## Verification and live follow-up

```sh
make verify-fast
go test -race ./internal/interactive ./internal/interactiveauth \
  ./internal/interactiveops ./internal/interactiveexec ./internal/invoke
go test -tags integration ./internal/interactiveexec \
  -run 'TestIntegration(Actual|Steering)' -count=1 -v
GOOBERS_INTERACTIVE_TEST_BINARY="$PWD/.tools/goobers" \
  go test -race -tags integration ./internal/interactiveops -count=1 -v
```

Portal acceptance uses a fresh local-tools server and the separate Playwright
configuration in `portal`; see [the browser acceptance instructions](../../portal/e2e/interactive-acceptance.md)
for its measured launcher and evidence. Keep screenshots, video, traces, logs and runtime state outside Git.
The handoff records exact commands, outcomes and paths. Broker tests cover
identity/resource isolation, decisions, refresh/challenge/revocation,
attenuation, concurrent replay and unknown writes using controlled providers.
They provide no evidence of live Microsoft delegated authentication.

A live Azure/ADO pilot needs explicit approval for tenant, resources and
permissions, verified issuer/audience/tenant/object identity, a frontend and
backend sign-in topology, consented ADO and Azure audiences, supported
authorization-code/OBO flows, encrypted token cache custody, Conditional
Access challenges, actual resource authorization, provider correlation and
revocation/uncertain-write qualification. None is provisioned here.

Decisions for the next iteration are the live sign-in and broker deployment
boundary, native-tool compatibility versus strict mediation, distributed
worker/lease persistence, production child-workflow lineage, and whether
collaborative sessions are part of the first live release.
