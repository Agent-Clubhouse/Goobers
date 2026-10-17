# Interactive helper operations prototype

The helper is an operating entry point into an **isolated local Goobers instance per authenticated actor/session**. It reads the fixture's current native configuration and run journals, reads the ADO-shaped `gaggle-context.json` backlog, proposes scoped changes, commits validated configuration, and submits actual declarative workflows. The backlog is explicitly a fixture; this package makes no live delegated-auth claim.

`Options.RepositoryForScope` should return the session execution workspace. This is host wiring, never a path accepted from the browser. Without it, `Repository` is copied into a private scope directory. Thus a workflow checks the session's current edited files, rather than a canned passing artifact.

## Native operations and boundaries

- `tweak_gaggle` and `tweak_workflow`: change only display name and/or enabled state.
- `add_workflow`: author a named manual workflow, using the native DSL schema/compiler validation. The prototype permits 1–8 deterministic stages, each at most 30 seconds, with the exact `node --test <session-repository>/invoice.test.mjs` argument vector, scratch workspace, and `network: none`. Credentials, scripts, arbitrary environment, provider mutations, schedule triggers, and extra control-flow kinds are refused.
- `submit_run`: invokes the built `cmd/goobers` binary through its native local scheduler with a stable `--request-id`. Config admission is separate from eventual workflow success. Durable trigger receipts bind the request to its run; the journal supplies observed phase and stage stdout/stderr.
- `stop_run`: records cancellation intent for an owned active workflow. Its action receipt confirms the request, not process termination. Poll the submitted run action for `cleanupConfirmed` and its observed result.
- `steer_run`: deliberately unsupported for deterministic subprocesses. Steer the interactive workspace through the shared session/execution service.

The fixture's project/backlog declarations are inert local identifiers; no provider credentials are configured. Native execution receives a private HOME and a minimal environment, with no ambient provider tokens. This proves the existing native workflow runtime, while keeping ordinary workflows unchanged. The process still runs as the host user: the fixture runner is not a general security sandbox for arbitrary untrusted repositories or arbitrary native CLI credentials.

## Decisions, recovery and admission

Requests have a stable actor/session-bound idempotency key and payload digest. Reusing a key for a different payload fails. Exact action Apply records the actor decision. Configuration previews pin the before and expected after generation digests. The native durable config transaction validates and swaps a candidate; after interruption, the helper observes committed versus restored generations. A changed or unrelated generation remains uncertain. An authorization or dispatch is never displayed as successful application.

Run actions persist a dispatch marker before spawning. After process loss, an unknown dispatch is not repeated, even with the same Apply request. Native scheduler receipts and journals may reconcile the observed result. Config changes between preview and Apply require a new proposal.

File locks serialize scope operations and native admission across **processes on one supported host**. `MaxActive` defaults to 4; `MaxActivePerOwner` defaults to 2; each session has at most one helper worker. Capacity exhaustion leaves the proposal prepared with `waiting: capacity`, so retrying Apply cannot duplicate a dispatch. Missing worker custody with an unresolved dispatch continues to reserve budget. These locks and local tests do not establish distributed/cloud scale. The shared session worker lease/fencing implementation is separate; native helper execution adds a bounded child-process budget and uses the native scheduler's instance lock and acceptance ledger.

The total native process lifetime defaults to 60 seconds and cannot exceed five minutes. `StopSession` first persists a scope-wide mutation fence, requests stop, then waits for released worker custody and confirmed process-tree cleanup. It returns an error for unknown custody. `Close(ctx)` applies this to all scopes in this service root and must be awaited during server shutdown. A force-stopped native run can retain an uncertain journal even when process cleanup is confirmed; neither side is rewritten into an invented successful completion. Disconnecting a browser does not call stop.

## HTTP contract

Mount `Service.Handler` at `/api/v1/interactive/operations/`, supplying a scope resolver that authenticates the actor and checks session membership/status. Resolve the session from `?sessionId=...`; never accept actor identity from request JSON. The host owns origin/CSRF policy and authorization against stopped or revoked sessions.

- `GET state`: current fixture state, proposed workflow draft, actions and observed runs.
- `POST proposals`: typed `Request` from `types.go`.
- `POST actions/{id}/apply`: exact reviewed action decision; no mutable action payload.
- `GET actions/{id}`: refresh/reconcile one operation.
- `POST chat`: `{text,idempotencyKey}`. This is an explicitly bounded local intent parser for common helper requests, not a language model. It returns the same reviewable proposals. Unknown explicit workflow names are rejected rather than redirected to a default.

## Verification

Ordinary package tests cover native config validation/application, owner/session isolation, payload replay, stale previews, policy rejection, committed/rolled-back recovery, unknown dispatch, HTTP identity binding, exact workflow targeting, stop fencing, and cross-instance capacity.

Native integration tests require a built Goobers binary and Node.js:

```sh
GOOBERS_INTERACTIVE_TEST_BINARY=/absolute/path/to/goobers go test -tags=integration ./internal/interactiveops -count=1 -v
```

They author/add a real workflow, observe a genuine failing Node test, edit the scoped repository, observe a passing rerun, and stop a genuinely running native task with process-tree cleanup proof. On macOS this requires permission to invoke Goobers' nested `network:none` sandbox and inspect owned process identities. A restrictive outer execution sandbox can make the native stage fail before Node starts; that is an environment failure, not a simulated pass. These tests never invoke a hosted model or live ADO/Azure API.
