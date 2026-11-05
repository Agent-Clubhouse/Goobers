# Design: trusted-local child execution

> Status: implemented — macOS/Linux preview; Windows strict custody refused
> Delivered-by: #7663
> Scope-delta: Windows runtime custody remains unqualified and refused; live harness qualification is separate.
> Verified: cf1da99bc858edde5284a2233acb4e88ccf03556 (2026-10-20)

Agent-authored DSL 3.1 children must work on a daemon host without Temporal,
pods, or distributed runners. Local and distributed execution share admission,
lineage, terminal results and workspace disposition. Placement determines where
an admitted attempt executes; it must not change the child's authority.

## Selection and compatibility

`trusted-local` names a daemon-owned execution provider, not a new DSL field or
an additional instance flag. Select it when the resolved instance runner
inventory contains only self placement and engine projection is disabled.
The existing workflow annotation
`metadata.annotations.goobers.dev/allow-preview-features: "true"` is the explicit
opt-in; child execution remains off without it. See the
[operational guide](../guides/trusted-local-child-workflows.md).
An explicit remote placement still fails ordinary placement admission. A
configured distributed runner that is offline does not enable local fallback.
Instances with distributed runners retain the contained worker provider.

Keep the retained `BackendRunner` coordinator identity and existing envelope
schema. Its stage provider admits every retained placement before execution.
Recheck current instance policy before effects; a configuration change must
never relocate already accepted distributed work onto the host.

The daemon still supplies durable authority, child handoff, capacity, journal
ownership and workspace recovery. Standalone or incompletely composed runners
remain refused. Parent admission accepts self placement in trusted-local mode
without requiring pod authentication or Linux image runners.

## Execution and scheduling

Compose source-bound child factories from the existing deterministic executor
and harness adapters. Retain the exact accepted workflow, Goober identities,
configuration generation, stage plan and credential ceiling. Do not register a
generated workflow as ordinary scheduled automation or restore provider hooks
removed by `Runner.ForChildExecution`.

Use managed repository forks and fresh worktrees for attempts, with the
existing OS sandbox wrapper and process ownership. Pinned checkouts cannot
substitute for owned worktrees. A retry starts from its recorded input rather
than reusing an uncertain writer's checkout.

Serialize local child attempts by default using a shared, cancellation-aware
permit. Acquire it at physical invocation, not while a parent waits or while
an accepted child is queued. Parent wait releases its ordinary capacity before
child admission and reacquires it for continuation. Parallel and map branches
retain their identities and result/join semantics even when physically serial.
Existing instance and workflow limits continue to apply.

## Credentials and trust

Trusted-local has the security posture of an explicitly configured local
Goober: it executes trusted workflow code under the daemon user's OS account.
It is not a container or a boundary against hostile code running as that user.
The existing sandbox wrapper remains in force; do not advertise equivalent
host isolation to private worker pods.

Credentials injected into a child must be a subset of the parent's delegated
credentials, intersected with current policy. Preserve `ChildCeiling` through
factory construction, credential resolution, invocation and publication.
Local selection is not permission to remove the existing authentication
isolation check globally. Only the admitted trusted-local provider may opt into
the host-process path, through a host-owned context marker rather than an
agent-supplied envelope field.

Use explicit child environments and private CLI configuration/home directories.
Delegate the parent stage's permitted authentication mode: a parent using an
ambient harness login may delegate that same selected harness login; a parent
using an explicit model key may delegate only the parent's exact selected
grant/reference and must never fall back to stored login or resolve another
stored secret. The host retains this binding for the stage occurrence and
checks every selected child profile before constructing its runtime. The instance and Goober policy remain authoritative;
refuse ambient authentication only where the parent itself would be refused.
Do not inherit unrelated provider tokens, other stored CLI logins, Git credential
helpers or unselected MCP credentials. Preserve restrictions such as Codex
keyring-only authentication when delegating the selected login.

A future extension may use the experimental orchestrator design's DSL-declared
`allowedAgentProfiles` ceiling to authorize a different harness/model. Both that
profile and its credential reference must be explicitly declared in the parent's
DSL ceiling and granted by the instance. V1 does not implement that extension;
selecting an otherwise allowed Goober is not authority to resolve another secret.
The retained authentication binding is the extension point.

Config discovery, launcher handshakes and authentication preflight are launches
too: use the invocation's delegated authentication scope or consume configuration
already resolved by admission. Unresolved child config must fail before an
adapter-wide credential resolver or ambient discovery can run. Publication uses
the existing canonical host publication owner under the same delegation ceiling,
never an ambient shell token. These controls bound credentials supplied by
Goobers; they do not make a shared OS account inaccessible to malicious host code.

## Custody and parity

Both normal return and cancellation stop and join the entire owned process
tree using process ownership on macOS/Linux. Windows Job Objects observe only
Job membership: WMI (`Win32_Process.Create`), WSL brokers, COM and service
launches can escape that membership. An empty Job must return a distinct
membership-only result, which strict workspace custody refuses. Use
`internal/invoke` workspace-writer registration and quiescence proof. A returned
result, cancelled context or dead direct process is insufficient evidence.
Persist the existing writer acknowledgement before capturing a snapshot,
disposing a checkout or permitting a replacement attempt.

Daemon loss with missing writer evidence retains custody and blocks new effects
until exact ownership and termination can be established. Never infer safe
recovery from a reusable PID, kill unrelated processes, or silently adopt an
unacknowledged checkout. Qualification must distinguish safe refusal after a
crash from successful automatic recovery.

Keep `TerminalResultInput`, accepted-source receipts, snapshots and
merge/replace/discard unchanged. Emit the existing child, wait, continuation,
attempt, placement and writer-custody events; self placement identifies local
execution. Pod dispatch/surrender events remain specific to physical pods.
The parent observes the same result and disposition contract in either mode.

## Delivery and validation

[#7663](https://github.com/Agent-Clubhouse/Goobers/pull/7663) delivered the local
parent lifecycle and child factory, including credential binding, default
serialization and workspace custody. Hermetic production-composition journeys
passed on macOS and in hosted Linux CI. These use fake model/forge adapters;
live harness releases and models require separate qualification.

Tests must exercise the production composition with fake model/forge adapters:
self-only selection, no fallback from remote placement, accepted child execution,
agentic and deterministic stages, parent wait/continuation, parallel serialization,
fresh retries, cancellation with descendants, credential narrowing, parent ambient-to-child ambient delegation and
parent explicit-key-to-child explicit-key enforcement, terminal snapshots, and all three dispositions.
Crash tests must prove unresolved writers retain custody and cannot be retried.

Run targeted race tests on macOS and Linux, `make build`, and a cross-GOOS
Windows build. A Windows cross-build checks portability, not Job Object runtime
behavior. Windows trusted-local execution remains unqualified preview and strict
workspace custody stays refused even after Job membership tests pass. Jobs must
prohibit breakaway before root execution; missing custody or unobservable
accounting fails closed. No preview override is introduced here. Any future
unqualified-preview opt-in must explicitly accept the brokered-launch escapes;
automatic local-mode selection cannot authorize it. No live model, cloud
resource, Temporal server or cluster is required for local qualification.
