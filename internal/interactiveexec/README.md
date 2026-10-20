# Interactive execution boundaries

The host pins execution mode, model, harness, persona, Gaggle, Goober, MCP tool
names and symbolic authority references on the admitted session request. A
`codex-local` Helper turn goes to the native adapter without deterministic text
matching. Unsupported harnesses fail closed. An absent model uses the legacy
host model default; HTTP sessions should supply their resolved policy.

`Config.ResolveAuthority` maps pinned symbolic references to exact owned broker
connection revisions. No references means no automatic provider context or
grant. Configured references require a trusted resolver, with no application
identity fallback. Existing session grants cannot acquire different connection
bindings. Provider reads use host-selected resources and execution identity,
lease and fence; model arguments contain only typed read inputs and stable
operation keys. The current provider implementation is explicitly controlled
local ADO and App Insights, not live delegated authentication.

`Config.AgentTools` constructs callbacks closed over the host request. Each
native invocation receives a private Unix MCP bridge exposing the intersection
of host tools and the pinned tool list. This list limits the host bridge only.
It does not restrict Codex's built-in filesystem or shell tools: `codex-local`
retains the trusted single-user workspace-write boundary of the existing
adapter. Tool names must not be copied into `harness.RunRequest.Tools`, whose
meaning is an unsupported native built-in-tool allowlist.

Stop and steering close the bridge and wait for admitted host callbacks as well
as the adapter's workspace writer proof. An uncooperative callback fails custody
closed, prevents a successful steering acknowledgement or a new invocation,
and retains the writer marker. Recovery cannot assume it has stopped; manual
worker isolation and reconciliation are required before clearing uncertain
custody. A late tool completion after a failed stop is not proof of successful
cancellation.

`native_bridge_test.go` injects all process/model callbacks while exercising real
Unix sockets, typed controlled provider calls, exact scope, replay conflicts,
and stop/steer lifecycle. These tests prove the host/adapter contract, not that a
model used the tools successfully. The separately tagged native model
integration test requires independent authorization and is not part of that
qualification.

Model execution uses the `internal/chat` registry, scoped to the pinned harness.
Copilot prefers its SDK transport; the existing adapters provide an execshim
fallback for each provider. The historical `codex-local` execution-mode name
still denotes the explicit trusted-local-login host boundary; it no longer
forces a Codex-only transport. There is no fallback across providers or into
the deterministic demo.

Native steering keeps the live driver session. Unsupported steering closes and
joins the old writer and bridge, then sends a replacement prompt over preserved
files. Delivery receipts report `steer`, `queued`, or `prompt` from the driver;
`prompt` steering additionally records `interrupt-and-continue`. A failed
replacement is rejected instead of claiming delivery after only a stop.

The host projects v2 events through interactiveagent.ProjectChatEvent. It closes
and verifies native custody before snapshot capture, storing the native session
identity in host metadata so a later leased turn can resume it. Copilot's own
session is long-lived across sends while an execution lease is active; a native
idle event alone never permits a snapshot while the process is still alive.
