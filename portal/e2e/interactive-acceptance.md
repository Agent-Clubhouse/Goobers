# Interactive prototype qualification

This suite contacts the real `interactive-prototype` HTTP server. It does not
substitute canned browser responses. One recovery test deliberately loses a
response after the real backend has accepted the command. Keep the evidence directory outside the repository.
The dedicated config avoids accidentally running these tests against the
ordinary portal fixture daemon.

After building the portal and starting the prototype server as described in
the prototype launch documentation, use a fresh state directory. The final
test permanently revokes a controlled ADO connection; re-running requires
fresh state or different controlled identities.

For a measured local-tools launch with startup metadata and server logs, run
this in one terminal after building `.tools/interactive-prototype`,
`.tools/goobers`, and the portal assets:

```sh
INTERACTIVE_EVIDENCE_DIR=/tmp/goobers-interactive-evidence \
node portal/e2e/interactive-server.mjs
```

The launcher copies `samples/interactive-config` into a fresh `config/` directory
under the evidence directory and passes it through `--config-dir`. Reload tests
edit this copy, never tracked configuration. The launcher refuses to reuse an
existing state/config directory or an occupied port and never adds a trusted
model opt-in. It verifies that model access and live-provider claims are disabled.
`INTERACTIVE_REPOSITORY_ROOT` can point it at a separate checked-out integration
branch. `INTERACTIVE_STATE_DIR` and `INTERACTIVE_PORT` override the state path and
loopback port. `INTERACTIVE_CONFIG_SOURCE` selects a source config tree to copy.

```sh
cd portal
INTERACTIVE_BASE_URL=http://127.0.0.1:8097 \
INTERACTIVE_EVIDENCE_DIR=/tmp/goobers-interactive-evidence \
npx playwright test --config e2e/interactive.config.ts
```

## Acceptance evidence boundaries

| Layer | What a passing result demonstrates |
| --- | --- |
| Browser | Real portal controls, HTTP command admission, event replay, responsive rendering, visible activity and errors. |
| Session backend | Durable IDs, command replay, conflicting payload rejection, stale fence rejection and preserved history. |
| Local tools executor | Actual checkout inspection, file edit, test process and cumulative diff; deterministic planning is explicitly disclosed. |
| Bounded child | A generated test-only `review-child` workflow runs through the native Goobers scheduler in an independent snapshot. The test checks its run ID, successful receipt, `network: none` definition and confirmed process cleanup. This is test evidence, not model semantic review. |
| Helper operations | Actual Goobers configuration validation and transaction, authored workflow submission through the native CLI, and observed journal completion in an isolated instance. |
| Controlled provider | ADO-shaped data, credential lifecycle and local authority enforcement against a fake provider. This is not a live Azure or ADO delegated sign-in. |
| Native Codex adapter | Only a separately recorded run with `--executor codex-local` demonstrates native model work. Default local-tools tests cannot establish that claim. |

## Required journey

1. Open both optional surfaces; verify Agent is unavailable and not live-qualified.
   Explicitly select Deterministic demo; create a private session with a stable ID
   and pinned `local-tools` execution mode. Missing/agent API mode must be rejected,
   never silently mapped to the demo executor.
2. Investigate the repository and scoped ADO-shaped context, identifying the
   controlled provider in the UI.
3. Edit the sample through execution, run its tests and review cumulative changes.
4. Deliver steering during an active turn. Distinguish accepted receipt from
   delivered acknowledgement and preserve the message after reconnect.
5. Request a bounded child, review its proposed work, run its native test-only
   workflow and review its result. Verify that child execution does not adopt
   changes into the parent snapshot.
6. Disconnect/reload and reconnect to the same session. History and event IDs
   persist without launching duplicate work.
7. Expire then reauthenticate the second controlled connection after the first
   provider read has completed; resume the same turn. Revoke the connection
   during active work, verify cancellation and test that a new session cannot
   bypass the revoked connection.
8. Stop the executor's active bounded-wait turn; wait for confirmed stop and
   retain workspace changes. Actual subprocess cancellation and process-tree
   cleanup are separately exercised by the native integration tests.
9. At narrow viewport widths, verify the message composer and critical controls
   remain accessible without document-wide horizontal overflow.
10. Use two explicitly controlled local personas to execute concurrently, reject
    cross-owner session reads/commands, and verify their workspace changes differ.
11. In Helper, design and apply a bounded native workflow, edit the gaggle display
    name, submit the new workflow, and observe the native completed run receipt.
12. Reload copied DSL configuration: reject invalid input without fencing active
    work; then reduce the child limit and verify the active old session is fenced,
    its worker releases custody, old-policy commands are rejected, and a new
    session can execute using the changed policy. Restore the copied configuration.

## Independent checks

Session creation and command retries reuse their logical identity. A changed
payload with the same ID is rejected. Stale session fences cannot create work.
Unknown provider effects remain uncertain instead of being retried as failed
operations. Cross-owner access, child authority attenuation, revocation races
and process recovery additionally require backend tests and source review;
the loopback demo principal by itself cannot establish multi-user isolation.

Screenshots and videos are evidence of observed states, not proof of live
provider access or production isolation. Record test command, branch/commit,
executor/provider mode and remaining limitations with each qualification run.
