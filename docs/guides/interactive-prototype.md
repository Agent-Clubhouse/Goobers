# Interactive sessions and delegated access prototype

`experimental/interactive` combines two optional portal experiences with durable
sessions, private working copies, actual local execution, and mediated delegated
access plumbing. Product configuration lives in the normal Goobers DSL tree:
Manifest, Gaggle, Goober and Workflow definitions. This experimental command is
separate from the production daemon; it is not a deployed cloud service or a
production authentication system. Ordinary workflows retain their existing path.

## Launch locally

Use the Go version pinned in `go.mod`, Node.js 24, npm and Git. Native workflow
execution needs permission to inspect its owned process tree and apply the
existing network restriction. An outer sandbox can prevent this; cleanup
uncertainty must remain visible rather than becoming a successful stop.

From the repository root:

```sh
npm ci --prefix portal
npm run build --prefix portal
mkdir -p .tools
go build -o .tools/goobers ./cmd/goobers
go build -o .tools/interactive-prototype ./cmd/interactive-prototype
.tools/interactive-prototype \
  --config-dir samples/interactive-config \
  --source samples/interactive-investigation \
  --state-dir /tmp/goobers-interactive-demo
```

Open <http://127.0.0.1:8097/#/interactive>. Select **Deterministic demo** explicitly
when creating a Helper or Workbench session. Both sample surfaces request
`codex-local` by default, but the host does not enable that adapter by default.
An unavailable Agent mode is an error; it never silently becomes the demo.
Execution mode is immutable for the session.

The source is a trusted host-selected local directory, copied into a private
session workspace. The sample has a real invoice bug and failing Node test.
Project metadata does not cause this command to clone a remote repository.
Keep state and browser evidence outside every Git checkout. Use fresh private
state for an independent demonstration. Ctrl-C requests cleanup; wait for its
result before reusing state. Restart preserves history but requires explicit
reauthentication of controlled connections. Revoked fixture connections cannot
be revived by restarting or opening another session.

Host flags select resources and operational boundaries: `--config-dir` (default
`samples/interactive-config`), `--source`, `--state-dir`, `--goobers-bin`, `--assets`,
`--listen` and `--local-actors`. The command binds only a literal loopback address.
There is no separate surfaces JSON file or executor, lease or admission-limit CLI
configuration; those product settings come from the DSL and remain subject to
host ceilings.

The server writes a one-use URL to `human-auth.url` inside its private state
directory and prints only the file path. Open that link in the human's browser
before using the portal. The file is removed after exchange or clean shutdown. The token is carried in the URL fragment,
removed from browser history before exchange, and consumed once. Restart the
server to revoke browser sessions or obtain a new bootstrap URL. Do not give
this URL or the browser's cookies to an agent, and keep console capture outside
agent-readable roots.

Every interactive API read and event stream requires the authenticated browser
cookie AND an independent `X-Goobers-CSRF` header, including reads and SSE.
The one-time bootstrap response delivers the header binding to origin-scoped
browser storage; it is never a cookie or URL parameter. Cookies are shared
across loopback ports, so a preview server capturing cookies still cannot read
or control sessions. The portal uses authenticated fetch streaming for SSE.
Host/Origin checks still apply. The session cookie is HttpOnly and SameSite=Strict.
Secrets are never added to harness environment, requests or workspace files.

The local persona selector is a controlled demonstration, not Entra sign-in.
Only the authenticated human may select a configured persona. Each browser
context receives an opaque server-bound persona cookie; a raw actor-name
cookie cannot choose an owner. Independent contexts can demonstrate owner
isolation. This does not provide multi-user remote authentication.

The authentication boundary depends on OS confinement: agents must not read
server state/logs, browser credential/storage profiles or host process memory. Writable agent
workspaces belong outside denied host state. A sandboxed subprocess fixture
verifies denied secret reads and rejects unauthenticated approve/apply requests
even when the loopback service is reachable. Network access alone is not human
authority; residual host IPC/browser access requires separate confinement.

## Configure the experiences in DSL 3.0

Start with [the complete sample](../../samples/interactive-config/README.md).
`Gaggle.dslVersion: "3.0"` governs only `spec.interactive`; the Gaggle itself
acknowledges that preview using
`metadata.annotations[goobers.dev/allow-preview-features]: "true"`. Each Workflow
keeps its own version and preview acknowledgement. Neither a Gaggle pin nor its
acknowledgement changes ordinary Workflow interpretation. Existing DSL 2.0
workflows can coexist with interactive 3.0. Omission, or `interactive: {}`, adds
no surfaces.

A surface binds an existing Goober, permitted mediated tools, same-gaggle
workflow references, local authority references, execution modes, session
ceilings and reviewed-child capabilities. The sample uses:

```yaml
name: helper
mode: helper
gooberRef: helper
executionModes: [codex-local, local-tools]
defaultExecutionMode: codex-local
tools: [session_context, inspect_action, validate_workflow, propose_action, propose_child_workflow, read_work_item, query_telemetry]
workflows: [invoice-check]
authorityRefs: [ado-context, telemetry-context]
session: {leaseSeconds: 30, maxTurnSeconds: 600, maxChildren: 3, maxActive: 4, maxActivePerOwner: 2}
childWorkflows:
  enabled: true
  commandCapabilities: [node-test, node-check]
```

Set a surface's `enabled` to false to disable admission. Names are scoped to the
Gaggle; runtime IDs are `<gaggle>--<surface>`. The normal loader validates names,
references, bounds and preview acknowledgement, then selects Manifest-active
Gaggles. Goobers can be gaggle-owned or use the existing instance-shared layout.
`Goober.spec.harness`, optional `model`, and the referenced Markdown instruction
file bind the agent. The sample chooses `codex` and omits `model`, preserving the
existing adapter default; set an approved model in the Goober when needed.

The prototype compiler supports Codex Goobers only. It explicitly rejects
unsupported nonempty Goober tools, external MCP servers, harness options,
skills, capabilities and policy-action grants rather than silently dropping
those declarations. Surface tools configure the mediated host bridge; they are
not Goober builtin-tool settings. Missing tool, workflow and authority lists
permit none. The DSL cannot enable a host-disabled execution adapter or create
credentials or grants.

The command validates a private snapshot through `instance.LoadConfigDir`.
Its revision hashes the configuration files and any adjacent instance-shared
Goober definitions/instructions. Supported snapshot assets are YAML and Markdown;
links, special files, unsupported assets, excessive sizes and recognized
credential-shaped content are refused. This is not a general secret scanner.
Sessions pin that revision and their resolved surface policy.

After editing the host's selected configuration, reload explicitly:

```sh
curl --request POST \
  --header 'Origin: http://127.0.0.1:8097' \
  http://127.0.0.1:8097/api/v1/interactive/config/reload
```

Invalid input retains the last known-good catalog. A changed valid revision
fences sessions pinned to the old revision, cancels active work and requires
confirmed writer custody before capacity is released. Start a new session to
use the new policy. History and safe terminal controls remain available; the
worker does not silently migrate old grants. A changed shared instruction file
also changes the revision. Configuration is not automatically watched.

## What executes, and what is still unqualified

| Layer | Implemented boundary |
| --- | --- |
| Portal/session backend | Real HTTP/SSE, durable history, steering receipts, reconnect, explicit stop and pending/error/auth states |
| `local-tools` | Explicit deterministic driver with actual source reads, edits, Git review and Node subprocesses; not a language model |
| Helper operations | Scoped native config validation/transactions, immutable human-reviewed proposals, native workflow admission and observed journals |
| Dynamic children | Exact reviewed bounded Workflow YAML, an independent workspace snapshot, native execution receipts and explicit result review |
| `codex-local` | Existing Codex harness adapter plus invocation-bound typed MCP tools; tested with controlled adapter/callback substitutes, live model qualification pending |
| ADO/App Insights | Controlled local providers with scoped authority and durable operation receipts; no live Microsoft API calls |
| Identity | Controlled local personas; no live sign-in, consent, credential acquisition or new grants |

The demo understands `investigate`, `fix` for the sample invoice bug, `test`,
`review`, `read <relative path>` and `wait`. Its Helper also offers bounded
configuration/run intents such as `gaggle status` and `design workflow`. These
are explicit demonstration controls. The native model path uses seven typed
MCP tools instead of that deterministic intent parser:

| Tool | Purpose |
| --- | --- |
| `session_context` | Read configured scope, workflows, observed actions/runs and labeled fixture backlog |
| `inspect_action` | Read an owned proposal or native execution receipt |
| `validate_workflow` | Validate authored bounded DSL and return diagnostics/normalized YAML |
| `propose_action` | Propose a workflow/configuration change or run control for human review |
| `propose_child_workflow` | Propose exact authored child YAML; never start or approve it |
| `read_work_item` | Read a scoped controlled ADO fixture through the broker |
| `query_telemetry` | Query scoped controlled App Insights observations through the broker |

The bridge binds actor, session, turn, lease and fence in the host. Calls cannot
choose those identities, inject authority, create grants or approve themselves.
Stable intent keys belong to the host operation ledger; a disconnected tool
client does not establish that an admitted effect did not occur.

Native model access additionally requires `--trusted-local-model-login` and an
existing local Codex login. It can transmit the selected workspace, configured
instructions and controlled provider observations to the model service. The
attempted live smoke was **denied before execution by automatic approval
review**, pending explicit approval of that payload and model destination.
Do not enable or claim qualification of this path until that action is approved
and its actual result is recorded. No live model call is required to validate
configuration or test the local bridge.

The native adapter uses a trusted local workstation boundary. Its prompt asks
for bounded behavior, but the mediated-tool allowlist does **not** restrict the
harness's builtin filesystem or shell tools. The bridge's private Unix socket is
not protection against a hostile process running as the same OS user. Existing
local model login is infrastructure authentication, not delegated Microsoft
identity. There is no application-identity fallback.

## Demonstrate the local paths

1. Explicitly select the demo and open Workbench. `investigate`, `fix`, `test`
   and `review` exercise real reads, edits, test output and cumulative changes
   without a backlog item or pre-authored workflow.
2. Send `wait`, then steer it. Observe accepted versus delivered steering.
   Disconnect/reload and reconnect to the same session. Replay must preserve
   event order without creating another execution.
3. Propose a bounded child, review its definition, and approve at an idle parent
   checkpoint. Review the observed native result. The child uses an independent
   snapshot; no patch is automatically adopted and the parent cannot write
   while a child is running.
4. In Helper, inspect `gaggle status`, design a bounded workflow, review/apply
   the exact proposal and submit it. Observe native run receipts, including
   failure before the invoice fix and success afterward. Configuration changes
   affect the session's isolated instance. Each run captures the repository at
   an idle session checkpoint and uses a separate execution instance. Its receipt
   records the captured repository, bound payload digest and native journal.
   Later chat edits and repository-relative test writes remain independent.
   Replaying Apply never captures another snapshot or dispatches another run.
5. Use two browser contexts and the controlled personas to compare private
   histories and workspaces. Cross-owner reads/commands must fail.
6. Expire or challenge a fixture connection, observe an auth wait, reauthenticate
   and resume. Revoke during work and verify cancellation. Reauth must not revive
   revoked authority, reset budgets or authorize a pending mutation.
7. Stop active work and retain partial edits. Distinguish stop requested,
   stopping, stopped and custody pending. Closing a browser is not a stop.
8. Reload an invalid copied config and verify no policy replacement. Reload a
   valid changed policy, observe fencing, and open a new session with it.

Authored child workflows use the bounded native DSL 2.0/3.0 path: manual trigger,
1–8 deterministic stages, explicit `node --test`/`node --check` file arguments,
scratch execution and 1–30 second stage deadlines. DSL 3.0 uses
`runsOn.restrictions: [network:none]` and its own preview acknowledgement;
DSL 2.0 uses `run.network: none`. Host path binding and the surface's command
capabilities apply before admission. This path does not establish production
DSL 3.1 `task.childWorkflows` lineage, arbitrary shell workflows, publication,
recursive children or distributed child scheduling.

## Durability, capacity and delegated authority

Stable command IDs bind exact payloads. Lost-response retries retain their
identity; stale sequence rejection permits a bounded refresh only before
admission. Unknown effects remain uncertain until their operation receipts are
reconciled. SSE sequence cursors reconnect without dispatching work.

Worker leases carry fences through event and broker admission. Idle sessions
need no worker; finished turns release capacity. Interrupted writers retain
capacity until custody is verified. Private writer markers live outside the
model-writable workspace. There is no force-resume path for an unknown writer.
One coordinator owns each atomic-file store; this is not a distributed queue,
Kubernetes worker provisioner or autoscaling implementation.

The sample requests four active executions and two per owner. The host also
caps interactive execution at four/two and native workflow workers at a separate
four/two. Surface ceilings may narrow admission further; children consume
capacity. The two pools can total eight workers. Owner/membership/actor fields
are explicit, but shared sessions and collaborative approvals are not enabled.

Named authority declarations are non-secret host broker references. This host
resolves only `controlled-local-ado` and `controlled-local-app-insights`.
Immutable grants bind exact resources/actions, an original deadline and shared
operation budget. Children attenuate that authority; stop/revocation fence it.
Provider admission rechecks current lease and policy. An already admitted effect
can still complete across a concurrent stop and must be reconciled from its
receipt. Tokens belong in broker custody, never prompts, logs or artifacts.

Native Azure CLI, Git helpers, arbitrary HTTP clients and token-consuming MCP
servers cannot transparently inherit mediated authority. They need an explicit
future adapter or separately approved custody boundary. See the
[broker contract](../../internal/interactiveauth/README.md) and
[native Helper operations](../../internal/interactiveops/README.md).

## Verification and remaining work

The assembled DSL integration passed ten browser acceptance scenarios against
the real local HTTP/SSE backend, including a native DSL 3.0 child, an authored
Helper run in its private snapshot, reload fencing and historical receipts.
The final integration also passed `make verify-fast`, the six-package backend
race suite and three actual Git/Node execution/custody integration tests.
The Helper's full native-plus-race suite and independent integration review
passed. The handoff records the exact source checkpoint, binary/asset hashes,
commands and outside-Git screenshots, videos, traces and logs.

Normal schema/reference/preview validation, `instance.LoadConfigDir`, generated
CRD installability, deep-copy isolation and config generation apply/replay/update
have passed tests. The apply test uses a fake Kubernetes transport; it changes
no cluster. Session and broker tests use controlled executors/providers. MCP
transport tests use real private sockets and controlled callbacks; native adapter
tests can substitute a harness. Local Git/Node/native workflow execution provides
actual execution evidence separately. None of these proves live model reasoning
or Microsoft delegated authentication. Subsequent code changes require appropriate
new qualification; earlier evidence describes only the recorded checkpoint.

Useful checks include:

```sh
make verify-fast
go test -race ./api/validate ./api/v1alpha1 ./internal/instance \
  ./internal/configsync ./internal/workflow -run TestInteractive -count=1
go test -race ./internal/interactive ./internal/interactiveauth \
  ./internal/interactiveagent ./internal/interactiveops ./internal/interactiveexec \
  ./internal/invoke ./cmd/interactive-prototype
go test -tags integration ./internal/interactiveexec \
  -run 'TestIntegration(Actual|Steering)' -count=1 -v
GOOBERS_INTERACTIVE_TEST_BINARY="$PWD/.tools/goobers" \
  go test -race -tags integration ./internal/interactiveops -count=1 -v
```

Use [the browser acceptance instructions](../../portal/e2e/interactive-acceptance.md)
with a fresh demo server and copied configuration. Store screenshots, video,
traces, logs and runtime state outside Git. Those artifacts show observed local
states, not live provider access or production isolation.

A live Azure/ADO pilot still needs approved tenant/resources/permissions,
verified identity, frontend/backend sign-in topology, consented audiences,
supported authorization-code/OBO flows, encrypted token cache custody,
Conditional Access challenges, real resource authorization and uncertain-write
qualification. Nothing here provisions those prerequisites. Other open decisions
are production daemon integration, distributed persistence/workers, native-tool
compatibility, production child lineage and collaborative sessions.
