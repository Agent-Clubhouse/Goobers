# Interactive helper operations prototype

The helper is an operating entry point into an **isolated local Goobers instance per authenticated actor/session**. It reads the fixture's current native configuration and run journals, reads the ADO-shaped `gaggle-context.json` backlog, proposes scoped changes, commits validated configuration, and submits actual declarative workflows. The backlog is explicitly a fixture; this package makes no live delegated-auth claim.

`Options.RepositoryForScope` should return the session execution workspace. This is host wiring, never a path accepted from the browser. Without it, `Repository` is copied into a private scope directory. Thus a workflow checks the session's current edited files. Custom repositories do not need invoice files: a suitable repository test/check supplies the initial draft, or the scope starts with no workflow when there is no supported target. The sample retains its invoice workflow.

## Native operations and boundaries

- `tweak_gaggle` and `tweak_workflow`: change only display name and/or enabled state.
- `add_workflow`: author a named manual workflow, using the native DSL schema/compiler validation. The prototype permits an authored, acyclic chain of 1–8 deterministic stages, each at most 30 seconds, with scratch workspace and `network: none`. Host-selected `CommandCapabilities` enable `node-test` (`node --test` plus 1–16 explicit relative `.test.mjs` files) and/or `node-check` (`node --check` plus one relative `.mjs` file). Nil enables both; an empty capability list enables neither. Credentials, scripts, arbitrary environment, extra Node flags, provider mutations, schedule triggers, loops, unreachable work, and other control-flow kinds are refused.
- `submit_run`: invokes the built `cmd/goobers` binary through its native local scheduler with a stable `--request-id`. Config admission is separate from eventual workflow success. Durable trigger receipts bind the request to its run; the journal supplies observed phase and stage stdout/stderr.
- `stop_run`: records cancellation intent for an owned active workflow. Its action receipt confirms the request, not process termination. Poll the submitted run action for `cleanupConfirmed` and its observed result.
- `steer_run`: deliberately unsupported for deterministic subprocesses. Steer the interactive workspace through the shared session/execution service.

The fixture's project/backlog declarations are inert local identifiers; no provider credentials are configured. Native execution receives a private HOME and a minimal environment, with no ambient provider tokens. This proves the existing native workflow runtime, while keeping ordinary workflows unchanged. The process still runs as the host user: the fixture runner is not a general security sandbox for arbitrary untrusted repositories or arbitrary native CLI credentials.

## Decisions, recovery and admission

Requests have a stable actor/session-bound idempotency key and payload digest. Reusing a key for a different payload fails. Exact action Apply records the actor decision. Configuration previews pin the before and expected after generation digests. A durable per-scope binding pins the canonical repository path and host command capabilities across restarts. Changing either requires a new scope and review. A target must be a regular file within that repository; path traversal, symlink components, metadata/dependency paths and foreign absolute paths fail validation. Existing same-scope absolute command paths can be normalized into portable relative paths. The native durable config transaction validates and swaps a candidate; after interruption, the helper observes committed versus restored generations. A changed or unrelated generation remains uncertain. An authorization or dispatch is never displayed as successful application.

Run actions persist a dispatch marker before spawning. After process loss, an unknown dispatch is not repeated, even with the same Apply request. Native scheduler receipts and journals may reconcile the observed result. Config changes between preview and Apply require a new proposal.

File locks serialize scope operations and native admission across **processes on one supported host**. `MaxActive` defaults to 4; `MaxActivePerOwner` defaults to 2; each session has at most one helper worker. Capacity exhaustion leaves the proposal prepared with `waiting: capacity`, so retrying Apply cannot duplicate a dispatch. Missing worker custody with an unresolved dispatch continues to reserve budget. These locks and local tests do not establish distributed/cloud scale. The shared session worker lease/fencing implementation is separate; native helper execution adds a bounded child-process budget and uses the native scheduler's instance lock and acceptance ledger.

The total native process lifetime defaults to 60 seconds and cannot exceed five minutes. `StopSession` first persists a scope-wide mutation fence, requests stop, then waits for released worker custody and confirmed process-tree cleanup. It returns an error for unknown custody. `Close(ctx)` applies this to all scopes in this service root and must be awaited during server shutdown. A force-stopped native run can retain an uncertain journal even when process cleanup is confirmed; neither side is rewritten into an invented successful completion. Disconnecting a browser does not call stop.

## HTTP contract

Mount `Service.Handler` at `/api/v1/interactive/operations/`, supplying a scope resolver that authenticates the actor and checks session membership/status. Resolve the session from `?sessionId=...`; never accept actor identity from request JSON. The host owns origin/CSRF policy and authorization against stopped or revoked sessions.

- `GET state`: current fixture state, proposed workflow draft, actions and observed runs.
- `POST validate-workflow`: `{workflowYaml}` returns `WorkflowValidation` with normalized diagnostics, portable `normalizedYaml`, host-resolved `boundYaml`, `payloadDigest`, `bindingDigest`, and per-stage command capabilities. Invalid YAML/policy returns `valid:false`; authentication/storage failures remain errors. Native DSL schema and graph compilation both run. Validation creates no action or workflow.
- `POST proposals`: typed `Request` from `types.go`. `add_workflow.workflowYaml` may contain a model-authored definition within this bounded subset. The immutable preview contains the exact bound execution definition; Apply revalidates it and native config validation runs before commit.
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

## Bounded native review children

`RunReviewChild(ctx, scope, requestID, prompt, emit)` executes an already-admitted parent's exact test-only review contract. The caller supplies a distinct child scope and independent repository snapshot. The compatibility wrapper renames the canonical validated draft to `review-child`, applies that native workflow, submits it through the same helper worker pool, and emits the immutable YAML plus real action/run/output receipts. The prompt is descriptive metadata bound to the idempotency key; it never supplies command arguments or workflow instructions.

`RunReviewChildWithOptions(..., ReviewChildOptions{WorkflowYAML, ApprovedPayloadDigest}, emit)` accepts an authored definition that was validated and reviewed by the parent. It preserves the exact task graph and authored name, rebinding only portable paths to the private child snapshot. `ApprovedPayloadDigest`, when supplied, must be SHA-256 of the exact normalized source bytes and must match revalidation. The event exposes both portable payload and private execution binding digests. The parent must approve before calling this host-only API; model tools must not receive an Apply/approval capability. The caller must keep the child snapshot immutable during execution. These file/path checks do not turn arbitrary repository JavaScript into safe untrusted code.

Capacity waits are bounded by the caller's context and an additional service timeout. Exact replay returns the original native run. A failed regression test is a normal returned action whose status is `failed`; uncertain effects, unconfirmed custody, admission failures, and cancellation return errors. Cancellation or an event sink failure calls `StopSession` for that exact child scope and waits for cleanup proof. It never calls `Close` on the shared service root or fences the parent scope.

Dynamic execution regressions use a custom repository with no invoice files, author a syntax stage followed by two selected test files, observe both genuine test results, replay the same native receipt, and prove a syntax error prevents downstream execution. Unit/contract coverage includes normalized digest stability, independent child bindings, approval mismatch before effects, scope swaps, restricted host capabilities, multiple YAML document rejection, unsafe flags/paths/symlinks, loops and unreachable stages. No hosted model or live delegated-auth result is claimed.
