# Copilot launcher session contract

`runner.harnessCommand` replaces a harness's launch prefix. A wrapper is not
necessarily compatible merely because it forwards arguments. Goobers supports
two generic integration paths:

- A launcher can implement the versioned handshake below.
- A launcher without the handshake can be admitted as `adapter-managed` only
  after the normal headless authentication preflight proves that a generated
  `--session-id` produces the corresponding non-empty native Copilot transcript.

For mode 3 stage pods, the worker carries only the selected goober's configured
argv in the content-addressed execution kit, for both task invocations and
reviewer gates. Each target image must provide that executable and any launcher
dependencies at the declared path. The pod performs its own normal launcher and
authentication preflight. Omitted overrides retain the default command. Deploy
matching updated worker and stage binaries: older stage binaries do not consume
the kit's launcher override.

The default command and an explicit `["copilot"]` retain the existing direct
Copilot behavior. Every other Copilot override is first probed using its complete
configured prefix followed by `--goobers-launcher-contract`. This probe must not
start an agent, contact a model, request credentials, or modify a session. It
returns exactly one JSON object on stdout, at most 16 KiB, within ten seconds.
Stderr diagnostics are captured separately and are never parsed as the contract.
A nonzero exit is treated as an absent handshake and proceeds to the behavioral
`adapter-managed` proof. Truncated output, malformed successful output, unknown
fields, and unsupported versions or modes fail closed before workflow dispatch.
Successful contracts and behavioral
proofs are cached for that Goobers process. Changing a wrapper requires
restarting that process. A separate worker or stage process performs
its own proof; verification is never written to configuration or shared storage.

Model discovery is not sent through a custom launcher. Goobers connects the
Copilot SDK directly to `copilot` for that server-mode exchange, then uses the
configured launcher for authentication preflight and workflow execution. This
keeps SDK transport and logged-in-user authentication owned by the SDK client
instead of requiring every launcher to proxy the Copilot server protocol.

```json
{"version":1,"sessionMode":"adapter-managed"}
```

Supported session ownership modes:

- `adapter-managed`: Goobers generates a fresh UUID, appends `--session-id UUID`,
  and reads the corresponding native Copilot session log. The wrapper must give
  this flag exactly the direct CLI's local-session meaning.
- `templated`: the wrapper declares `sessionArgs`, for example
  `["--local-session", "{sessionId}"]`. Goobers substitutes a fresh UUID as data,
  never shell syntax, and appends those arguments instead of `--session-id`.
  The wrapper must map this ID to the usual native Copilot session log under
  `COPILOT_HOME`. Only `{sessionId}` is supported; arguments must be nonempty,
  at most 1 KiB each, with at most 16 arguments.
- `wrapper-managed`: Goobers adds no session selector. The wrapper owns its
  internal session identity and must export that invocation's native Copilot
  JSONL events to the absolute `GOOBERS_SESSION_TRANSCRIPT` path in its environment.
  This is a private per-invocation path within the workspace, not a request to
  select a remote task. Goobers reads it using the existing bounded native-event
  reader and removes the temporary export after capture. A completion-recovery
  invocation receives the same path and must continue the same logical session.

Wrapper-managed and templated modes reject conflicting direct session selectors
already present in the configured invocation. No search for the newest session
or another run's transcript is performed. All modes retain the existing prompt,
completion, usage, model, tool, credential, and sandbox contracts; this is not a
new harness type. The wrapper must also support version and authentication probes
on its complete configured launch prefix.

The handshake is an explicit compatibility declaration. The behavioral fallback
is proof only of direct local session forwarding, not of every launcher feature.
Configured launchers therefore use native-session usage accounting and do not
receive optional Copilot flags inferred only from the reported CLI version.
Validate a new launcher end to end with a harmless workflow in its target OS and
isolation posture.

## Durable partial transcripts

Goobers checkpoints newly captured process output and the selected native
Copilot log once per minute. The process-output capture defaults to 4 MiB;
bytes beyond the capture limit are counted as dropped. Unchanged output does
not create another checkpoint. Process exit, cancellation, and timeout request
a final delta flush; an abrupt kill can only preserve checkpoints already
acknowledged by the journal.

A wrapper must append native events to its selected log while it runs, not
export the entire log only at exit. Once Goobers observes that file, replacing,
truncating, or removing it is a capture error. Completion-recovery turns must
continue the same native log. Process-output invocations retain separate delta
cursors so recovery output does not overwrite the first invocation.

Use `goobers trace --transcripts <run-id> [path]` to retrieve recorded output.
Incomplete captures appear as `.transcript.partial` spans with their stream and
reason (`checkpoint`, `process-exit`, `canceled`, or `timeout`). Each span carries
only a delta, in durable journal order; this is not a live follow interface.
Streaming redaction withholds ambiguous suffixes until it can safely publish
them, so a partial may omit an unfinished token rather than expose a credential.

Successful finalization commits the canonical transcript before deleting its
private partial blobs. Checkpoints write only new safe bytes, not repeated full
snapshots. Partial metadata remains in the journal, but normal transcript reads
hide superseded partials and successful stages retain no extra checkpoint blobs.
If a final blob is missing or corrupt before cleanup, surviving partials remain
accessible through the sequence-addressed transcript read API.

Workers and pods use the existing run-scoped journal connection for durable
acknowledgments. A configured blob store carries the final transcript; without
one, the final bytes travel inline over the journal connection. This does not
grant additional authentication scopes. Checkpoint and finalization calls have
separate 10-second and 30-second deadlines. Failed or uncertain checkpoint writes
are surfaced as capture errors rather than silently advancing the source cursor.
