# Interactive prototype qualification

This suite contacts the real `interactive-prototype` HTTP server. It does not
mock browser responses. Keep the evidence directory outside the repository.
The dedicated config avoids accidentally running these tests against the
ordinary portal fixture daemon.

After building the portal and starting the prototype server as described in
the prototype launch documentation:

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
| Local tools executor | Actual checkout inspection, file edit, test process, cumulative diff and bounded child process; deterministic planning is explicitly disclosed. |
| Controlled provider | ADO-shaped data, credential lifecycle and local authority enforcement against a fake provider. This is not a live Azure or ADO delegated sign-in. |
| Native Codex adapter | Only a separately recorded run with `--executor codex-local` demonstrates native model work. Default local-tools tests cannot establish that claim. |

## Required journey

1. Open both optional surfaces; create a private session with a stable ID.
2. Investigate the repository and scoped ADO-shaped context, identifying the
   controlled provider in the UI.
3. Edit the sample through execution, run its tests and review cumulative changes.
4. Deliver steering during an active turn. Distinguish accepted receipt from
   delivered acknowledgement and preserve the message after reconnect.
5. Request a bounded read-only child, review its proposed work, run it and review
   its result. Verify that child execution does not silently adopt changes.
6. Disconnect/reload and reconnect to the same session. History and event IDs
   persist without launching duplicate work.
7. Expire then reauthenticate the controlled connection; a pending request must
   not claim success. Revoke the connection and verify new effects are denied.
8. Stop an active process; wait for confirmed stop and retain workspace changes.
9. At narrow viewport widths, verify the message composer and critical controls
   remain accessible without document-wide horizontal overflow.

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
