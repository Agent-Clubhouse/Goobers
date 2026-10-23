# Stage CLI capability checks

Codex and Copilot preflight check the fixed subset of public options listed below
before model dispatch. They query bounded stdout from `codex exec --help`
or `copilot --help` with a ten-second timeout. A newer version string cannot make
an absent option pass, and an older version is accepted when it advertises the
listed options. Failure names the missing flags and recommends upgrading or
configuring a compatible launcher. These extra probes do not send model prompts
or resolve model credentials.

| Adapter | Public options checked by this probe |
| --- | --- |
| Codex | `--json`, `--ignore-rules`, `--disable`, `--sandbox`, `--skip-git-repo-check`, `--cd`, `--config` |
| Codex ambient ChatGPT | Above plus `--ignore-user-config` |
| Copilot native defaults | `--allow-all-tools`, `--log-level`, `--log-dir`, `--session-id`, `--no-remote-export` |

Option-definition lines must appear in stdout; stderr and prose mentioning an
option do not count. Empty, oversized, timed-out and unsuccessful help requests
fail verification. Executables named `codex` or `copilot`, including Windows
shims, use this check. Custom commands, prefixed launchers and custom Copilot
argument builders return unobservable capability evidence; their negotiated
launcher contracts remain responsible for compatibility.

`PreflightInfo.Checks` separates public option support from authentication.
An API key's presence does not verify provider authentication or model
entitlement. Copilot without a configured authentication probe has unknown auth
status even when its version/help checks pass. Existing configured Copilot auth
probes and Codex ChatGPT login-status checks retain their own guarantees.

This is a partial capability check, not a minimum-version promise or a guarantee
of every protocol. Public help does not establish configuration-key semantics,
model support or hidden RPC behavior. In particular, working Copilot releases
omit `--headless` from public help; its support must be established by the
controlled-session handshake. Claude's existing preflight remains unchanged.
The read-only readiness endpoint remains separate and does not invoke Copilot's
optional prompt-based authentication probe.

The complete capability floor remains tracked in
[#7621](https://github.com/Agent-Clubhouse/Goobers/issues/7621). This subset does
not close that issue. Its remaining work includes:

- Checking every public option required by the selected invocation, with
  option-dependent fixtures. The current probe does not check Codex `--model`
  or Copilot `--model`, `--available-tools`, `--silent`, `--output-format`,
  `--additional-mcp-config` and `--disable-builtin-mcps`. Passing this probe
  therefore does not establish support for every option later used by a stage.
- Establishing Claude's capability checks and evidence-based supported
  CLI/version floors across all three adapters. Version observations alone do
  not establish a supported minimum.
- Checking required protocol responses and configuration semantics, including
  the controlled Copilot session protocol and unsupported-build fixtures.
- Establishing authentication usability and model entitlement separately from
  credential presence, while keeping read-only readiness probes free of model
  calls. Custom launchers continue to report unobservable native capability
  evidence and require their own contract checks.
