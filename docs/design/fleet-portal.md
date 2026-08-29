# Design: Fleet portal and control-plane gateway

> Status: **draft**
> Scope: fleet-level human and API entry point, instance registration, request routing,
> authentication, authorization, multi-instance observability, and target-scoped
> interactive diagnostics
> Related requirements: [`portal.md`](../requirements/portal.md),
> [`instance.md`](../requirements/instance.md),
> [`security.md`](../requirements/security.md), and
> [`deployment.md`](../requirements/deployment.md)

## 1. Summary

A Goobers **fleet** SHOULD have one human and API entry point. That entry point serves
the fleet dashboard, authenticates users, authorizes access, and routes permitted
requests to individual Goobers instances.

The entry point is a separate logical service, the **fleet portal**, rather than a
portal deployed independently for every instance. Each instance explicitly enrolls
with one fleet URI and maintains an outbound authenticated connection to it. Browsers
and API clients talk only to the fleet portal; they do not need network reachability
to an instance.

A fleet contains one or more **fleet groups**. A fleet group is the non-nested
permission boundary below a fleet and above an instance. It is deliberately named
plainly rather than introducing another product metaphor:

```text
fleet
  fleet group
    instance
      gaggle
        goober
```

For example, one corporate fleet could contain `payments`, `commerce`, and
`developer-sandboxes` groups. Users receive fleet-wide or group-scoped roles, and
instances inherit the access policy of their group.

This is a proposed change to the current architecture. Today the portal is defined as
a window into one instance and explicitly not a control plane. If this design is
approved, the architecture and Portal, Instance, Security, and Deployment requirements
must be updated before implementation issues are treated as build-ready.

## 2. Product intent

### 2.1 One entry point per fleet

Every fleet has one canonical HTTPS base URI, for example:

```text
https://goobers.example.com
```

That origin hosts:

- the fleet dashboard;
- the fleet registry and health API;
- group and instance discovery for authorized users;
- the versioned proxy surface for instance read and runtime-operation APIs;
- target-scoped interactive Copilot diagnostic sessions for authorized operators;
- enrollment, credential rotation, and instance revocation APIs; and
- the event or connection endpoint used by enrolled instances.

The fleet URI is a deployment identity, not merely a convenient link. Instances pin
the fleet identity discovered at this URI and use it as the audience for fleet-issued
credentials.

### 2.2 A gateway, not a second configuration system

The fleet portal is the control-plane **entry point** for people and API clients. It
does not become a second source of workflow, gaggle, goober, or instance
configuration. Config remains code, and the instance remains the authority for its
runtime state and policy enforcement.

The fleet portal may expose runtime operations already permitted by the versioned
instance API, such as gate approval, retry, cancel, or abort. It MUST NOT invent a
parallel orchestration path or mutate configuration outside the existing config-as-code
contract.

### 2.3 Separate service, flexible placement

The fleet portal is a separate logical service with its own identity, lifecycle,
storage, and availability boundary. It can be deployed in several ways without
changing the protocol:

| Mode | Intended use |
|---|---|
| Central corporate fleet | One highly available service for an organization or business unit |
| Team fleet | One shared service to which team-owned instances and developer boxes enroll |
| Co-located fleet | A small deployment in the same Kubernetes cluster as one or more instances |
| Local development fleet | A loopback-only process or container for one developer |

Co-location is a deployment optimization, not an in-process trust shortcut. The fleet
portal and instance still use the same authenticated registration and routing
protocol. A local instance may also remain completely fleetless and use its existing
CLI or per-instance loopback portal.

## 3. Terminology and ownership

### 3.1 Fleet

A fleet is the top-level human access, API, identity-provider, and audit boundary. It
has:

- one canonical URI;
- one user identity-provider configuration;
- one fleet signing identity;
- one or more fleet groups;
- a registry of enrolled instances;
- an authorization policy; and
- an audit log.

A large organization MAY run multiple fleets when it needs genuinely separate
identity, compliance, region, availability, or administration boundaries.

### 3.2 Fleet group

**Fleet group** is the provisional name for the permission boundary below a fleet. A
group:

- has a stable ID and URI-safe slug;
- contains zero or more instances;
- has group-scoped role assignments and policy;
- is not nested in another group; and
- does not imply that its instances share runtime state, credentials, repositories, or
  Kubernetes infrastructure.

An instance belongs to exactly one fleet group at a time. Moving it between groups is
an audited administrative operation that immediately changes its inherited access
policy.

`Gaggle` is not reused for this concept. A gaggle is a workforce inside one instance,
while a fleet group contains instances and exists to scope human access.

### 3.3 Instance identity

An instance creates and persists a stable random instance ID locally. The ID is not
derived from a hostname, cluster name, filesystem path, or display name. Enrollment
binds that ID to one fleet and one fleet group.

An instance MAY change its display name without changing its identity. Recreating an
instance from configuration does not silently assume the identity of a deleted
instance; identity recovery or replacement must be explicit.

## 4. Architecture

```text
                         OIDC provider
                              |
                              v
browser / API client --> fleet portal and API
                         |  authn / authz
                         |  fleet registry
                         |  audit log
                         |  aggregate read model
                         |
                         | authenticated logical channels
                         | over instance-initiated connections
                  +------+--------------------+
                  |                           |
                  v                           v
             instance A                  instance B
             group: team-a               group: team-b
             local / VM / AKS            local / VM / AKS
```

Instances initiate every network connection. The fleet portal does not require a
public, routable, or fleet-reachable listener on an instance.

The logical protocol has two complementary paths:

1. **Live request proxy.** An authorized fleet request is sent over the instance's
   active channel to its versioned product API. The response returns through the same
   channel.
2. **Projected fleet read model.** Instances publish a deliberately bounded stream of
   health, capability, attention, and run-summary events. This supports fleet and group
   overview pages without fan-out requests to every instance and lets the UI show the
   last known state of an offline instance.

The projected read model is not the source of truth for an instance. Detailed or
mutation-sensitive operations use the live request path unless a later contract
explicitly defines safe asynchronous behavior.

## 5. Fleet API and routing

### 5.1 URI shape

The initial URI shape SHOULD make fleet, group, and instance scope explicit:

```text
GET  /api/v1/fleet
GET  /api/v1/groups
GET  /api/v1/groups/{group}
GET  /api/v1/groups/{group}/instances
GET  /api/v1/groups/{group}/instances/{instance}
ANY  /api/v1/groups/{group}/instances/{instance}/proxy/{instance-api-path}
POST /api/v1/groups/{group}/instances/{instance}/diagnostic-sessions
GET  /api/v1/groups/{group}/instances/{instance}/diagnostic-sessions/{session}
GET  /api/v1/groups/{group}/instances/{instance}/diagnostic-sessions/{session}/stream
POST /api/v1/groups/{group}/instances/{instance}/diagnostic-sessions/{session}:close
POST /api/v1/enrollments
POST /api/v1/instances/{instance}/credentials:rotate
POST /api/v1/instances/{instance}:revoke
```

The exact paths remain an API-design decision. The important contract is that scope is
unambiguous and that the proxied instance API remains versioned independently from the
fleet API.

### 5.2 Request envelope

The fleet portal MUST NOT turn a user request into an anonymous trusted backend call.
Every proxied request carries a signed, short-lived delegation envelope containing at
least:

- fleet ID and issuer;
- request ID and issue/expiry times;
- authenticated user subject;
- effective fleet and group roles;
- target instance ID;
- allowed operation or capability;
- HTTP method and normalized target path; and
- a body digest for mutation requests.

The instance validates the envelope, target, expiry, signature, and requested
capability before executing the request. The instance remains the final authorization
boundary and MAY apply stricter local policy. This prevents a compromised or
misconfigured proxy from silently bypassing instance authorization.

Delegation envelopes are single-request or narrowly replay-bounded credentials. They
are never general bearer tokens that can be reused to access other instances.

### 5.3 Offline behavior

If an instance is offline:

- fleet and group views show its last known summary and `lastSeenAt`;
- the UI clearly labels projected data as stale;
- live reads fail as unavailable rather than silently returning stale data;
- synchronous runtime mutations fail rather than being guessed successful; and
- queued commands are out of scope until their durability, expiry, cancellation, and
  duplicate-execution semantics are designed.

Instance scheduling and execution continue while the fleet is unavailable. Fleet
availability must not become a runtime dependency for work already configured at an
instance.

## 6. Enrollment and registration

### 6.1 Discovery

The fleet URI exposes an unauthenticated metadata document:

```text
GET /.well-known/goobers-fleet
```

It returns only public protocol metadata:

- fleet ID;
- canonical URI;
- protocol versions;
- enrollment endpoint;
- connection endpoint;
- signing-key discovery URI;
- supported authentication methods; and
- server capabilities.

It does not expose fleet groups, users, instances, run data, or policy.

### 6.2 Enrollment flow

The initial explicit flow is:

1. A fleet or group administrator creates a single-use, short-lived enrollment grant
   bound to a fleet group and an allowed initial capability set.
2. An instance administrator runs a command such as:

   ```text
   goobers fleet join \
     --url https://goobers.example.com \
     --enrollment-token-file <protected-file>
   ```

3. The instance retrieves discovery metadata and displays the canonical fleet identity,
   target group, requested data scopes, requested command capabilities, and certificate
   or key fingerprint.
4. After explicit confirmation, the instance proves possession of its locally generated
   key and redeems the enrollment grant.
5. The fleet returns the assigned instance registration and a renewable,
   instance-scoped credential.
6. The instance pins the fleet ID, canonical URI, and signing-key authority, then opens
   the outbound channel.

Managed environments MAY replace the enrollment grant with workload-identity
attestation, but the resulting registration has the same scope and audit record.

Enrollment grants are secret, single-use, expire quickly, and never appear in
`instance.yaml`, command history, logs, or the config repository. The fleet URI and
non-secret instance registration ID MAY be configuration.

### 6.3 Connection

The instance maintains an outbound mTLS HTTP/2, WebSocket, or equivalent
application-layer channel to the fleet. The transport remains an implementation
decision, but it MUST provide:

- mutual peer authentication;
- connection and request-level identity;
- multiplexed live requests and event publication;
- bounded queues and backpressure;
- heartbeat and last-seen semantics;
- credential rotation without re-enrollment;
- reconnect with bounded exponential backoff; and
- clean revocation behavior.

The fleet never instructs an instance to open an arbitrary URL or proxy an arbitrary
destination. Routing is limited to the registered instance product API.

### 6.4 Leave, move, and revoke

- **Leave:** an instance administrator removes the local association and credential.
- **Revoke:** a fleet administrator immediately disables an instance credential and
  closes active channels.
- **Move:** a fleet administrator moves an instance to another group with an explicit
  audit event and immediate policy re-evaluation.
- **Re-enroll:** joining another fleet first requires leaving or revoking the existing
  association. Multi-fleet registration is out of scope for the first version.

## 7. Authentication and authorization

### 7.1 Human authentication

The fleet portal owns human OIDC integration. Instances do not need separate browser
client registrations or public login redirects.

Recommended issuer postures are:

| Fleet posture | Recommended identity configuration |
|---|---|
| Corporate | Tenant-specific Microsoft Entra ID with Conditional Access |
| Public/community | Entra External ID or another stable OIDC issuer supporting consumer identities |
| Local development | Loopback-only local trust or a development issuer |

The browser uses Authorization Code with PKCE. A browser client secret is never
embedded in portal assets.

### 7.2 Role scopes

The initial role model has three scopes:

- **Fleet roles** manage fleet-wide policy, groups, identity configuration, and
  enrollment.
- **Group roles** grant `view`, `operate`, or `admin` over every current and future
  instance in one fleet group.
- **Instance grants** are optional exceptions for a particular instance and SHOULD be
  used sparingly.

Roles retain the current ordering:

```text
admin > operate > view
```

Fleet administration does not automatically grant permission to read every instance's
run data. Fleet infrastructure administration and fleet-data access SHOULD be
separable roles in production.

Starting or attaching to an interactive diagnostic session requires a distinct
`diagnose` capability in addition to ordinary `view`. The capability MAY be included
in an `operate` or `admin` role by policy, but it is independently auditable and
revocable. Read-only diagnosis and mutation-capable diagnosis are separate grants.

### 7.3 Effective authorization

For each request, the fleet portal:

1. validates the user token;
2. resolves fleet, group, and optional instance grants;
3. checks the fleet API or instance capability being requested;
4. records an audit decision;
5. creates a narrowly scoped delegation envelope; and
6. routes the request to the target instance.

The instance independently validates the delegation and applies local restrictions.
A deny at either layer denies the request.

## 8. Data boundaries

### 8.1 Data an instance publishes

Registration starts with a minimal metadata scope:

- stable instance ID and display name;
- group registration;
- Goobers version and supported API/capability versions;
- deployment tier and coarse platform;
- readiness, heartbeat, and last-seen state; and
- explicitly enabled projected summary fields.

Additional projections, such as run summaries or attention counts, require an explicit
scope in the registration. Full journals, artifacts, transcripts, repository content,
prompts, credentials, and secrets are not replicated by default.

Live proxy access does not imply permission to retain the response centrally. Fleet
storage and retention are separate, explicit contracts.

### 8.2 Data an instance receives

An instance receives only:

- fleet public metadata and signing keys;
- its own registration and policy version;
- requests addressed to that instance;
- revocation, rotation, and connection-control messages; and
- optional group policy relevant to local enforcement.

It does not receive the fleet directory, other instance metadata, other groups' data,
user directory exports, or unrelated authorization policy. A compromised instance
therefore cannot use registration as a fleet-discovery API.

### 8.3 Registration is still a trust decision

The fact that an instance receives no other fleet data is a valuable non-disclosure
property, but it does **not** make enrollment security-neutral.

By joining a fleet, an instance may disclose its own metadata and projected runtime
data, and it agrees to evaluate requests delegated by that fleet. A malicious fleet
could observe those projections, deny service, or attempt unauthorized commands.
Enrollment therefore MUST be explicit, authenticated, scoped, auditable, reversible,
and pinned to the intended fleet identity.

The safest initial enrollment is metadata-only and read-only. Runtime mutation
capabilities require a separate explicit grant. An instance MUST never auto-enroll
merely because a fleet URI is present in untrusted repository configuration.

## 9. Security model

### 9.1 Trust boundaries

The design treats these as distinct principals:

- human or API user;
- browser public client;
- fleet portal;
- fleet administrator;
- group administrator;
- instance;
- instance administrator; and
- workload identities used by stages.

Fleet identity is not a substitute for workload identity. Fleet credentials never
grant repository, model, secret-store, or sandbox access.

### 9.2 Required controls

- TLS on every non-loopback connection.
- OIDC validation at the fleet edge.
- Short-lived signed delegation validated again by the instance.
- mTLS or equivalent proof-of-possession credentials for instance channels.
- One-use enrollment grants and rotation without downtime.
- No direct public instance API, NodePort, or per-instance ingress requirement.
- Request and response size limits, deadlines, rate limits, and backpressure.
- Strict target/path normalization to prevent SSRF and confused-deputy routing.
- Per-session resource, duration, concurrency, tool, namespace, and egress limits for
  interactive diagnostics.
- Append-only audit events for enrollment, policy changes, access decisions,
  mutations, diagnostic session lifecycle and tool actions, movement, rotation, leave,
  and revocation.
- Secret redaction in fleet logs, traces, and error responses.
- Explicit retention and deletion policy for centrally projected data.

### 9.3 Compromise containment

A compromised instance can publish false data about itself but cannot impersonate
another instance, enumerate the fleet, or receive requests for another instance.

A compromised fleet portal is serious: it can observe centrally retained projections
and attempt delegated requests. Instance-side capability validation, short credential
lifetime, local policy, and audit reduce but do not eliminate that trust. Production
fleets therefore require hardened identity, key storage, isolation, backup, and
incident-response procedures.

## 10. Dashboard experience

The fleet dashboard reuses the existing calm, observability-first portal language. It
adds hierarchy rather than replacing the instance workbench.

### 10.1 Fleet overview

- fleet health and portal version;
- groups visible to the current user;
- instances needing attention;
- disconnected or version-incompatible instances;
- active and recently completed runs across authorized groups; and
- enrollment or credential-health warnings visible to administrators.

### 10.2 Group view

- group-scoped instance inventory;
- group attention and run summary;
- effective access and group administrators; and
- enrollment controls for authorized group administrators.

### 10.3 Instance view

Selecting an instance presents the existing per-instance information architecture:
overview, workflows, runs, run detail, journal timeline, artifacts, approvals, and
permitted runtime operations.

The UI always shows the active fleet, group, and instance scope. Mutation confirmation
includes the target instance to reduce cross-instance operator mistakes.

### 10.4 Diagnose

An authorized operator can open **Diagnose** from an instance view. The surface is an
interactive conversation with a Copilot CLI session running in the target instance's
environment or Kubernetes cluster. It is intended to answer the same operational
questions an owner would investigate from a local terminal: why a workload is
unhealthy, which configuration is active, what recent events show, and which safe
remediation is appropriate.

The page always displays:

- target fleet, group, instance, cluster, and namespace;
- diagnostic profile and effective capabilities;
- whether the session is read-only or mutation-capable;
- session owner, start time, expiry, and connection state;
- commands and tool calls proposed or executed by the session; and
- transcript retention and audit posture.

The diagnostic surface is not a fleet-wide chatbot. Every session is explicitly bound
to one target instance and one diagnostic capability grant.

## 11. Interactive Copilot diagnostic sessions

### 11.1 Goal and local parity

An operator SHOULD be able to diagnose an instance through the fleet portal with the
same Copilot-assisted workflow available when working locally. The Copilot process
runs at the target, where it can use explicitly granted diagnostic tools and observe
the target's current state. The fleet portal carries the authenticated conversation
and tool-event stream; it does not copy cluster credentials into the browser or run
cluster commands itself.

Every remotely initiated Copilot process runs inside a mandatory sandbox. Model and
harness tool controls are defense in depth, not the confinement boundary. If the
instance cannot establish and verify the required sandbox, the session fails closed
before Copilot starts; there is no remote "trusted environment" bypass.

The target session uses the release-matched Copilot CLI and Goobers agent toolkit so
local and remote diagnosis share instructions, command semantics, redaction, and
versioning. A session reports its Copilot CLI, toolkit, Goobers, and Kubernetes
versions before accepting work.

This capability is a deliberate, narrow amendment to the current portal design's
"not a chat client" boundary. The portal does not become a general assistant,
configuration author, or workflow chat surface. Conversation exists only inside an
explicit, target-scoped operational diagnostic session.

### 11.2 Session lifecycle

The initial lifecycle is:

1. An operator selects an instance and a fleet-approved diagnostic profile.
2. The fleet authenticates the operator, resolves the `diagnose` capability, and sends
   a signed start request over the instance channel.
3. The instance validates the delegation and local policy.
4. The instance creates an ephemeral diagnostic runtime in the target environment.
5. The runtime starts Copilot CLI with the approved toolkit, tools, limits, and
   target-scoped context.
6. The fleet proxies the bidirectional conversation and structured tool events to the
   browser.
7. The session ends on explicit close, idle timeout, maximum lifetime, revocation,
   operator disconnect policy, or runtime failure.
8. The target destroys ephemeral credentials, files, and compute; the fleet finalizes
   the audit record according to retention policy.

Starting a session is never an implicit side effect of viewing an instance. Reopening
an instance page does not silently resume or create a diagnostic runtime.

### 11.3 Target runtime

For Kubernetes instances, the preferred runtime is an ephemeral pod or Job in a
dedicated diagnostic namespace or the instance's control namespace. It MUST have:

- a dedicated service account;
- a read-only root filesystem and non-root user where supported;
- explicit CPU, memory, ephemeral-storage, duration, and concurrency limits;
- no host PID, host network, host path, privileged mode, or Docker socket;
- a default-deny network policy with only required model, fleet, and Kubernetes API
  egress;
- no inherited stage, repository, model, or secret-store credentials;
- a pod and session identity unique to that diagnostic session; and
- automatic cleanup independent of a clean browser disconnect.

For local or VM instances, the equivalent runtime is a constrained child process or
sandbox owned by the instance. It uses the same diagnostic profile and session
protocol rather than a separate remote-administration design.

#### The sandbox is a mandatory security boundary

Copilot, its model output, invoked tools, and all target-derived content are untrusted.
Tool allowlists and confirmation prompts do not replace an independently enforced
sandbox: a compromised harness, prompt-injected model, vulnerable tool, or malformed
target response must still be contained.

Each diagnostic session receives a fresh sandbox and MUST NOT reuse another session's
process, writable home, Copilot state, temporary files, service-account token, or
credential material. The sandbox enforces all of these dimensions:

- **Identity:** a unique session identity with only the diagnostic profile's
  permissions. Kubernetes service-account tokens are projected, short-lived,
  audience-bound, and omitted entirely when Kubernetes API access is unnecessary.
- **Filesystem:** a fresh writable scratch root; target configuration, journals, logs,
  and artifacts mounted read-only when access is granted; no host filesystem, other
  instance volume, container runtime socket, SSH agent, user home, or secret-store
  mount.
- **Process:** private process namespace; no host PID/IPC; no privilege escalation,
  setuid binaries, added Linux capabilities, device access, or ability to signal
  processes outside the sandbox.
- **Network:** default-deny ingress and egress. Permit only the exact fleet stream,
  Copilot/model endpoint, DNS path, and target APIs required by the selected profile.
  Access to cloud metadata endpoints and unrelated cluster or internet destinations
  is denied.
- **Kubernetes:** a restricted Pod Security posture plus explicit seccomp and, where
  available, AppArmor or equivalent mandatory access control. Namespace and RBAC
  constrain API reads independently from network reachability.
- **Resources:** hard CPU, memory, process-count, file-size, ephemeral-storage,
  bandwidth, session-duration, idle, and concurrency limits.
- **Lifecycle:** deletion on close, expiry, revocation, fleet disconnect, or instance
  restart, with a controller-side deadline so cleanup does not depend on the Copilot
  process cooperating.

The instance performs a bounded sandbox preflight before admitting a session and
records the mechanism and policy digest in the audit event. Admission fails if the
mechanism is absent, degraded, or cannot prove the required controls. Production
validation MUST include adversarial confinement probes that attempt filesystem writes,
cross-session access, credential reads, process escape, forbidden network egress, and
out-of-scope Kubernetes API operations.

Local and VM diagnostic sessions follow the existing platform-neutral sandbox seam and
[ADR 0001](../adr/0001-agentic-sandbox-mechanism.md): Seatbelt on supported macOS hosts,
bubblewrap on supported Linux hosts, or a stronger approved mechanism. Unlike a
locally initiated stage where an owner may knowingly choose a documented trusted-local
posture, a session initiated through the fleet portal has no unsandboxed fallback.

### 11.4 Diagnostic profiles

A diagnostic profile is fleet-approved policy, not free-form browser input. It defines:

- target scope, such as one namespace, instance control resources, or cluster-wide
  metadata;
- Kubernetes RBAC and Goobers API capabilities;
- allowed executable tools and subcommands;
- filesystem roots that may be read;
- allowed network destinations;
- resource and time limits;
- whether proposed mutations require confirmation or are forbidden;
- transcript and tool-output retention; and
- redaction rules.

The default profile is read-only and namespace-scoped. It can inspect workload status,
events, logs, Goobers health and version information, effective non-secret
configuration, rollout state, resource pressure, and recent instance journal
summaries.

Cluster-wide reads, secret metadata, pod exec, filesystem collection, packet capture,
and every mutation are excluded unless a stricter profile explicitly grants them.
Secret values are never a diagnostic capability.

### 11.5 Tool execution and approval

Copilot output is untrusted until an allowed tool action passes deterministic policy.
The runtime:

- parses tool invocations into structured requests;
- rejects tools and arguments outside the active profile;
- records the exact normalized command or API operation;
- applies output size limits and redaction before streaming;
- treats logs, events, repository content, and workload output as untrusted prompt
  input; and
- never interprets model text itself as authorization.

Read-only actions allowed by the profile MAY execute immediately. Mutations require:

1. a mutation-capable diagnostic grant;
2. deterministic instance-side policy approval;
3. an explicit operator confirmation showing the exact target and action; and
4. a short-lived, single-action delegation bound to the action digest.

High-risk actions such as changing cluster RBAC, reading secret values, disabling
policy, creating privileged workloads, or broadening the session's own permissions
remain forbidden even with ordinary `admin`. A separately designed break-glass process
is required for exceptional access.

### 11.6 Copilot identity and credentials

The session requires a supported Copilot entitlement and authentication mechanism.
The design MUST NOT copy a developer's long-lived local Copilot credential into the
cluster or reuse a shared fleet-wide personal token.

Supported production mechanisms must provide a short-lived, session-scoped identity or
an organization-managed workload entitlement. Until such a mechanism is available,
remote Copilot sessions remain an explicitly experimental capability and must use a
credential flow that is visible to and initiated by the operator.

Copilot credentials are:

- scoped to one session;
- held only in the target runtime;
- mounted or delivered through the platform secret mechanism;
- excluded from transcripts, logs, crash dumps, and fleet projections; and
- revoked or destroyed when the session ends.

The fleet portal never receives repository, model, cluster, or instance credentials.

### 11.7 Streaming and reconnect

Conversation, incremental model output, tool proposals, approvals, stdout/stderr, and
session state use a structured multiplexed stream over the existing outbound instance
channel. The protocol distinguishes model text from tool events and audit events;
clients never infer executed actions by scraping terminal text.

A browser reconnect MAY reattach to a still-live session after reauthorization. It
does not extend the session lifetime or bypass a revoked capability. If the instance
or fleet channel is lost, mutation approval and execution stop fail-closed.

### 11.8 Audit and retention

At minimum, the audit record contains:

- fleet, group, instance, runtime, user, and session IDs;
- effective diagnostic profile and capability digest;
- start, attach, detach, expiry, close, and cleanup events;
- model and toolkit versions;
- normalized tool proposals, authorization decisions, confirmations, and outcomes;
- mutation request and response digests; and
- redaction and truncation indicators.

Conversation retention is configurable and distinct from the mandatory security audit.
Organizations MAY retain no model conversation after session close while retaining
structured tool and authorization events. Retention policy is shown before the
operator starts the session.

## 12. Deployment profiles

### 12.1 Central production fleet

A production fleet SHOULD have:

- at least two fleet portal replicas;
- a durable relational registry and authorization store;
- a durable projected read model and audit log;
- managed TLS and key storage;
- OIDC with conditional-access policy;
- private backend services;
- public WAF or private corporate ingress according to posture;
- backup, restore, rotation, and disaster-recovery procedures; and
- independent scaling for user/API traffic and instance connections.

Instances require outbound HTTPS connectivity to the fleet URI but no inbound route
from the fleet.

Diagnostic sessions additionally require target-side runtime capacity, approved
diagnostic images, model endpoint egress, and a credential mechanism that meets
section 11.6.

### 12.2 Team or co-located fleet

A team fleet MAY run one replica and one durable volume or small database. It still
uses normal OIDC, enrollment, delegation, and audit contracts. Co-locating the fleet
portal with instances in one Kubernetes cluster does not permit bypassing those
contracts. A co-located diagnostic runtime still receives a distinct service account,
network policy, and capability grant.

### 12.3 Local development fleet

A local fleet MAY:

- bind only to loopback;
- use local trust rather than OIDC while it remains loopback-only;
- store registry and projected state in a local file or SQLite database;
- enroll one or more local instance roots; and
- be launched and removed with a developer-oriented command.

Publishing or tunneling that listener changes the trust boundary and requires TLS and
OIDC. There is no insecure remote-listener override.

Local diagnosis MAY launch the same Copilot/toolkit session as a constrained child
process and attach through the fleet UI. Fleetless local diagnosis remains available
through Copilot CLI directly.

## 13. Compatibility and versioning

Fleet and instance APIs version independently. Registration exchanges:

- fleet protocol versions;
- instance product API versions;
- event projection versions;
- supported runtime-operation capabilities;
- supported diagnostic-session, toolkit, and tool-event protocol versions; and
- minimum/maximum compatible versions.

The fleet portal MUST degrade by capability. An older instance can remain visible even
when it cannot support a newer operation. The UI disables unsupported actions with a
literal explanation rather than attempting them.

Protocol incompatibility does not stop the instance runtime. It marks fleet access as
degraded and preserves local CLI operation.

## 14. Relationship to the current architecture

This design preserves these existing decisions:

- journals remain the instance source of truth;
- config remains code;
- the portal remains observability-first;
- runtime operations stay deliberately narrow;
- the CLI remains sufficient for local operation;
- instances remain self-hosted and independently operable; and
- local and Temporal runners remain behind the same product contract.

It changes or extends these decisions:

- the natural team/cloud portal becomes fleet-scoped rather than instance-scoped;
- the portal service gains registry, group authorization, routing, and aggregate-read
  responsibilities;
- the fleet portal is a control-plane gateway, although not the configuration or
  workflow-execution source of truth;
- instances gain an optional outbound enrollment and connection client; and
- the portal gains a narrowly scoped diagnostic conversation surface, revising the
  existing blanket "not a chat client" position without adding general-purpose chat.

Approval requires corresponding amendments to `ARCHITECTURE.md` and the Portal,
Instance, Security, and Deployment requirements. Until then, those sources remain
normative and this document remains a proposal.

## 15. Delivery slices

Implementation issues SHOULD be filed only after the design and owning requirements
are approved.

1. **Protocol foundation:** fleet identity, discovery, stable instance identity,
   enrollment, rotation, revocation, and capability negotiation.
2. **Connection and proxy:** outbound instance channel, request envelope, live read
   proxy, deadlines, backpressure, and instance-side validation.
3. **Identity and policy:** OIDC, fleet/group roles, policy evaluation, delegation,
   and audit.
4. **Fleet read model:** bounded projections, heartbeat, stale/offline behavior, and
   retention.
5. **Fleet dashboard:** fleet, group, and instance navigation over the existing portal
   workbench.
6. **Diagnostic sessions:** target-side Copilot runtime, diagnostic profiles, secure
   streaming, tool policy, operator approval, redaction, audit, and cleanup.
7. **Local/team packaging:** loopback process, co-located Kubernetes deployment, and
   join/leave operator commands.
8. **Production hardening:** HA, database, key management, backup/restore, WAF/private
   ingress, observability, scale, and disaster recovery.
9. **Migration:** preserve direct local CLI/portal behavior and provide an opt-in path
   for existing instances.

## 16. Open questions

1. Is **fleet group** the final product term, or should the permission boundary be
   called a project, organization, namespace, or team?
2. Is the outbound transport HTTP/2 streaming, WebSocket, or another protocol?
3. Which projections are enabled by default beyond identity, version, and health?
4. Does the first version permit instance-specific role exceptions, or only fleet and
   group assignments?
5. Which runtime mutations are safe for synchronous proxying in the first release?
6. Does a production fleet retain detailed run data, or fetch it live and retain only
   summaries?
7. How are enrollment and instance identity recovered after loss of local state?
8. What is the minimum durable store for a co-located team fleet?
9. Should public anonymous viewing be a sanitized publication surface separate from
   the authenticated fleet portal?
10. Which existing portal API paths can remain byte-compatible behind the fleet proxy?
11. Which Copilot authentication mechanism can issue short-lived, session-scoped
    credentials to a target runtime without copying a developer's local credential?
12. What is the first supported read-only diagnostic profile and Kubernetes RBAC
    contract?
13. Which diagnostic tool events and conversation content must be retained, and for how
    long?
14. Is mutation-capable diagnosis in the initial scope or a later, separately approved
    hardening phase?
15. How should a local Copilot session and a fleet-proxied session share toolkit
    instructions and capability declarations without drifting?

## 17. Non-goals

- Replacing config-as-code with portal configuration.
- Moving workflow execution into the fleet portal.
- Providing a general fleet chatbot, configuration-authoring assistant, or unscoped
  remote shell.
- Sharing repository, model, backlog, sandbox, or secret-store credentials with the
  fleet.
- Giving the fleet portal cluster credentials or executing diagnostic tools in the
  fleet service itself.
- Falling back to an unsandboxed Copilot process when diagnostic-session confinement
  is unavailable or fails preflight.
- Allowing a diagnostic session to read secret values, escape its target scope, or
  broaden its own permissions.
- Allowing instances to enumerate other instances or fleet users.
- Requiring fleet availability for configured work to continue.
- Supporting one instance enrolled in multiple fleets in the first version.
- Making an arbitrary fleet safe to join without explicit trust and scoped consent.
