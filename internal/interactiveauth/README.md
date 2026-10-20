# Interactive delegated-access prototype

This package is a runnable **controlled-local provider demonstration**, not live Microsoft delegated authentication. It performs typed ADO-shaped and App Insights-shaped operations against local fixtures. The ADO comment adapter actually commits to its local durable provider ledger. No Microsoft sign-in, token, app registration, consent, role assignment, infrastructure change, or live provider write happens here. Every connection and result identifies this mode explicitly.

The session/executor owner uses the service directly; this package does not trust HTTP bodies as identity or issue web sessions. `LocalPrincipal(serverOwner)` accepts an identity already selected by a trusted local server adapter. Its constructor and issuer label do not claim Entra verification. `Principal` has no exported fields and JSON cannot manufacture one. The local server must restrict origin/host, establish its own browser session, and reject caller-supplied owner names.

## Integration

```go
broker, err := interactiveauth.Open(interactiveauth.Config{
    Path: "/outside/repository/authority.json",
    CheckExecution: func(ctx context.Context, actor interactiveauth.Identity, op interactiveauth.OperationRequest) error {
        return sessions.CheckExecution(actor.Subject, op.SessionID, op.TurnID, op.LeaseID, op.ExecutionFence)
    },
})
// Handle err; close the single-owner store when the server shuts down.
principal, err := interactiveauth.LocalPrincipal(serverAuthenticatedOwner)
ado, err := broker.ConnectDemo(ctx, principal, interactiveauth.ADO)
insights, err := broker.ConnectDemo(ctx, principal, interactiveauth.AppInsights)
authority, err := broker.IssueAuthority(ctx, principal, interactiveauth.AuthorityRequest{
    SessionID: sessionID, ExecutionID: sessionID, ExecutionFence: execution.Fence,
    Bindings: []interactiveauth.Binding{
        {Connection: ado.Ref, Resources: ado.Resources},
        {Connection: insights.Ref, Resources: insights.Resources},
    },
    AllowedActions: []interactiveauth.Action{interactiveauth.ReadWorkItem, interactiveauth.QueryTelemetry},
    NotAfter: time.Now().Add(time.Hour), Budget: 40, MaxChildren: 2, MaxDepth: 1,
})
// Handle each error. Root authorities can be reused across ordinary turns.
operation, err := broker.Execute(ctx, principal, interactiveauth.OperationRequest{
    Authority: authority.Ref, SessionID: sessionID, ExecutionID: sessionID,
    ExecutionFence: execution.Fence, TurnID: execution.TurnID, LeaseID: execution.LeaseID,
    OperationID: stableOperationID, ActionID: stableActionID, IdempotencyKey: stableOperationID,
    Action: interactiveauth.ReadWorkItem, Resource: interactiveauth.DemoADOResource(),
    Payload: interactiveauth.Payload{WorkItemID: 427},
})
```

The callback runs under the broker mutex immediately before durable dispatch. It must fail closed if the session store cannot prove the current writer lease/fence. Session lifecycle code must not acquire the broker mutex while holding its own lifecycle mutex. The actor passed to it comes from broker-verified authority ownership, not the operation request. Standalone broker tests omit this callback; that does not qualify session custody.

For an exact ADO mutation, the authority must include `AddComment`. An authenticated owner explicitly approves the immutable `OperationRequest` through `Authorize`; its `Decision.ID` then goes in `OperationRequest.DecisionID`. Consent or reauthentication alone is never an action decision. Expired decisions are not renewed by replaying `Authorize`; a fresh explicitly reviewed action/operation is needed if dispatch never began. A dispatched or uncertain operation must retain its identity and be reconciled.

ADO resource scopes are exact organization/project/repository tuples. Azure resource scopes are exact subscription/resource-group/App-Insights tuples. The controlled fixture contains only ADO work item 427, concerning excessive invoice discounts, and supports only the query `requests | summarize count() by resultCode`. There is no generic URL proxy, arbitrary KQL, cross-resource query, PAT source, or application-identity fallback.

## Lifetimes, recovery, children, and effects

- Authorities are immutable revisioned records. Connection binding revisions change only for a security binding change; this prototype never changes bindings in place. Renewal and reauthentication preserve binding revisions.
- `SetDemoAuthState(..., "expired")` demonstrates silent renewal. `interaction_required` and `consent_required` demonstrate auth waits. `Reauthenticate` only restores the same local owner's availability. Revoked connections remain revoked. On server restart, availability becomes `interaction_required`; no durable credential cache is invented.
- The prototype accepts authority deadlines no more than 24 hours ahead, at most 10,000 mediated operations, at most 16 children per authority, and depth at most four. These are bounded experiment limits, not accepted product defaults. The server chooses a narrower policy.
- `DeriveChild` can only reduce actions, resources, deadline, and child limits. It preserves the shared operation budget and a live parent edge. Parent stop, supersession, or expiry blocks old children. The child count remains cumulative across recovered parent revisions.
- After verified writer recovery, the trusted host may call `RebindExecution` with a strictly newer execution fence. It creates the next immutable version of the same authority ID, preserving its original deadline, scope, and consumed budget. The old revision and children still bound to it are fenced. Existing decisions do not transfer. Stopped, expired, and revoked grants cannot be revived this way.
- `StopAuthority` durably fences that revision. `RevokeConnection` durably fences use of the connection before acknowledging success. The session controller is responsible for stopping its local execution processes and reflecting custody separately.
- Final admission is serialized with revocation. Dispatch is persisted before the provider boundary. A request already admitted can finish after revocation; the package does not claim to retract a request already sent. Each receipt captures the admitted authority and connection epochs.
- Stable operation IDs are global and cannot be reused for different intent. Idempotency keys are unique within an authority ID across its revisions. Concurrent replays return the existing record and consume no new budget. Replays of historical receipts are allowed after revocation because they perform no effect.
- `SimulateLostResponse` deliberately loses the controlled provider's reply after its local commit. The operation is `outcome_unknown`; replay returns that state without sending again. `Reconcile` only reads the controlled provider's durable receipt. Missing evidence stays unknown. A crash with a durable dispatch marker also recovers as unknown.
- State uses a single-process file lock, private atomic file replacement, file/directory fsync, and fail-closed behavior after an uncertain store write. Store files belong outside git. It is not a distributed ledger.

There are no actual credential values to leak in this implementation. Public types, persistence, operation receipts, model inputs, and event APIs have no token field. Credential-pattern checks reject obvious credentials in comment/query payloads as a defense in depth. Real provider acquisition must put its private cache behind a separately reviewed custody boundary rather than adding token values to these DTOs.

## Qualification and remaining live work

`go test -race ./internal/interactiveauth` exercises the real Go broker, file lock, atomic persistence, identity isolation, exact target checks, expiry/renewal/challenge handling, revocation races, parent/child attenuation, cumulative shared budget, exact decisions, concurrent replay, unknown outcomes, restart recovery, and current-execution admission. Provider behavior in all these tests is controlled local behavior. These tests demonstrate neither live Entra authentication nor Azure/ADO delegated permission.

A live pilot still requires explicitly approved tenant/ADO/Azure resources, a chosen frontend/backend sign-in topology, validated issuer/audience/tenant/object claims, server-side authorization-code or supported OBO acquisition, correctly consented audiences, encrypted durable token custody with isolated service permissions, provider request correlation, Conditional Access/challenge handling, provider-side scope qualification, and restart/revocation/uncertain-write qualification against the actual APIs. No such access is inferred or provisioned by this package.

This is mediated access. Unmodified authenticated Azure CLI, Git credential helpers, arbitrary HTTP libraries, and token-consuming MCP servers are not compatible with this boundary. Native tools can call the broker's typed operations via a future host adapter, but receive no reusable Microsoft token. A workstation mode handing tokens to arbitrary tools would have a different security and revocation claim and needs separate approval and qualification.
