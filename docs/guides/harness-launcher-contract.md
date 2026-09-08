# Copilot launcher session contract

`runner.harnessCommand` replaces a harness's launch prefix. A wrapper is not
necessarily compatible merely because it forwards arguments. In particular,
`agency copilot` has been observed to interpret `--session-id` as a remote task
identifier rather than a local transcript correlation ID. It is not a supported
drop-in example. Use direct Copilot unless the wrapper implements this contract.

For mode 3 stage pods, the worker carries only the selected goober's configured
argv in the content-addressed execution kit, for both task invocations and
reviewer gates. Each target image must provide that executable and any launcher
dependencies at the declared path. The pod performs its own normal launcher and
authentication preflight. Omitted overrides retain the default command. Deploy
matching updated worker and stage binaries: older stage binaries do not consume
the kit's launcher override.

The default command, and an explicit `["copilot"]`, retain the existing direct
Copilot behavior. Every other Copilot override must respond to its complete
configured prefix followed by `--goobers-launcher-contract`. This probe must not
start an agent, contact a model, request credentials, or modify a session. It
returns exactly one JSON object on stdout, at most 16 KiB, within ten seconds.
Stderr diagnostics are captured separately and are never parsed as the contract. Nonzero exit,
truncated output, unknown fields, or unsupported versions/modes fail admission
and preflight before workflow dispatch. Successful contracts are cached for that
adapter instance. Changing a wrapper requires rebuilding/restarting that instance.

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

The handshake is an explicit compatibility declaration, not proof that an
arbitrary wrapper implements it correctly. Validate a new wrapper end to end
with a harmless workflow in its target OS and isolation posture. Do not declare
`adapter-managed` just to bypass an incompatibility error: that restores the
very session-ID assumption the gate prevents.
