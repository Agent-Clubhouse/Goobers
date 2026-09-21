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

## Environment isolation

Harness subprocesses inherit only Goobers' built-in environment allowlist plus
names explicitly configured in `runner.envPassthrough`. If a forwarding
launcher must not inherit a parent-process variable, list its name in
`runner.harnessEnvUnset`:

```yaml
runner:
  harnessEnvUnset:
    - OUTER_LAUNCHER_SESSION_ID
  harnessCommand:
    copilot: ["forwarding-launcher", "copilot"]
  harnessSessionArgs:
    copilot: ["--session-file", "{sessionId}.jsonl"]
```

Removals apply to harness execution, version/authentication preflight, and
admission-time harness probing. They do not affect deterministic stages or
scoped credentials injected for declared capabilities. An absent variable is a
no-op. If a name appears in both `envPassthrough` and `harnessEnvUnset`, removal
wins for harnesses while deterministic stages still receive the passthrough.

`runner.harnessSessionArgs` is an explicit alternative to the command-based
launcher-contract probe. Goobers substitutes a fresh UUID for `{sessionId}`,
appends the resulting literal arguments to the configured launcher, and reads
the corresponding native Copilot transcript. Use it when a launcher already
has a stable session argument but cannot implement `--goobers-launcher-contract`.
Only the Copilot harness currently supports this setting.

Model discovery is not sent through a custom launcher. Goobers connects the
Copilot SDK directly to `copilot` for that server-mode exchange, then uses the
configured launcher for authentication preflight and workflow execution. This
keeps SDK transport and logged-in-user authentication owned by the SDK client
instead of requiring every launcher to proxy the Copilot server protocol.

A configured command that is the Copilot CLI itself under another path —
`/usr/local/bin/copilot`, say — is not a wrapper, and discovery keeps using it.
Only a genuine launcher is bypassed, so pinning the CLI by absolute path does
not silently add a requirement that `copilot` also be on `PATH`.

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
Goobers separately probes whether a launcher accepts the version-supported
`--usage-output-file` option together with `--version`, without starting an
agent. Launchers that pass receive authoritative usage-file accounting;
launchers that do not retain native-session usage accounting. Validate a new
launcher end to end with a harmless workflow in its target OS and isolation
posture.

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

## Required MCP readiness before model dispatch

Direct Copilot invocations with the default arguments on macOS and Linux use
one owned headless process and one native SDK session for both the required
`goobers-io` check and model turns. Startup and each readiness phase have a
15-second cap within the invocation's total timeout. The adapter initializes
that session's tools, checks the server's connection, and requires all five
`goobers-io` tools in its inventory before sending the model prompt. A separate
throwaway MCP connection is not readiness evidence for this session.

The controlled session's permission handler permits only the declared tools,
keeps file requests within the workspace and the sandbox's existing narrow
linked-worktree Git grants (including symlink checks), and does
not approve URL access, managed approvals, or sandbox bypass. `--allow-all-tools`
is not treated as `--allow-all-paths` or `--allow-all-urls`. Custom permission
arguments keep the ordinary CLI execution path. Unsupported or ambiguous
permission requests fail closed in this unattended session.

A `runner.annotation` with kind `required-mcp-readiness` records the `server`,
`source`, `category`, `connection`, `inventory`, and `authorization` observations
with phase `before-model`, schema version 1, and the adapter identity. Server
errors, tool responses, credentials, and task content are excluded. Bounded
per-stage conditions are projected into the existing read model and status run
summaries without opening journals. A verified recovery clears an active
condition; unobservable authorization cannot clear an earlier authorization
failure. The 1.0.66 partial check can clear only transport/tool-availability
failures. Truncated or absent observations are explicit coverage limitations.
Fleet consumers must combine observations in time order within the same
workflow/stage/branch/adapter/server context, so a newer run's recovery can
clear an older run's failure without clearing a different stage.

When the runtime supports native tool execution, the adapter invokes only
`get_run_info` through the session's authorization pipeline. This read-only
probe creates no input-inspection receipts or artifacts. An observed denial is
`tool_authorization_failure`; a server requiring authentication is
`authentication_failure`. These are ordinary failed attempts, not free
infrastructure retries. Missing servers/tools and transport failures stop
before a model turn and use the runner's existing bounded infrastructure retry
allowance, independently of policy attempts. Exhausting that allowance fails
the stage; the adapter does not add an internal retry loop.

Copilot CLI 1.0.66 supports the connection and inventory checks but returns
JSON-RPC method-not-found for the native authorization probe. For that specific
structured response, the adapter records connection and inventory as `ready`,
authorization as `unobservable`, and the overall category as
`check_unobservable`, then permits the turn on the same session. This preserves
working deployments while preventing the observed registered-but-absent case;
it cannot promise to prevent authorization surprises on that runtime. Other
probe errors do not take this compatibility path. A fully `ready` observation
requires a successful native authorization probe.

Claude, Windows Copilot, custom Copilot launchers, and custom Copilot arguments
retain their existing execution paths and explicitly report
`check_unobservable`. Their existing post-turn checks remain in place. Direct
controlled Copilot sessions also inspect their actual server list after the
turn, because CLI-global lifecycle logs do not reliably describe SDK sessions.
After any completion-recovery turn, a bounded five-second finalization collects
session usage and gracefully shuts down the native session before reading
native captures. The usage RPC preserves per-model accounting even when a
persistent headless session has not yet written its ordinary CLI shutdown
record. Missing or invalid usage is not invented; capture/finalization errors
remain visible to the stage.

The opt-in read-only live checks send no model prompt:

```sh
GOOBERS_COPILOT_MCP_READINESS=1 go test -tags integration ./internal/harness \
  -run '^TestIntegrationCopilotRequiredMCP' -count=1 -v
```

They require an installed Copilot CLI and check absent-server rejection,
connection/tool inventory, native authorization capability, and post-turn
session evidence. The always-on adapter tests cover model-dispatch counts,
authorization rejection, timeout bounds, and the partial-observation policy.
