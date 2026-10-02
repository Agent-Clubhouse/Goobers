# MCP readiness in unattended preflight

`goobers preflight --instance <path> --workflow <name> --check-readiness --json`
includes an `mcp` row for each agentic stage's built-in `goobers-io` server and
declared external MCP servers. Without `--check-readiness`, preflight remains a
static report and launches no MCP session.

For a supported direct Copilot CLI on Unix, the check opens a disposable control
session in a temporary workspace, initializes tools, and reads the session's
server registry and tool inventory. It executes exactly one built-in operation:
`goobers-io.get_run_info`. It sends no model prompt. The complete session has a
60-second ceiling, with bounded initialization, settling and inventory phases,
and closes its owned process and removes its temporary files afterwards.

Each row separates the configured server name and transport, required tool
names, observed registry identity, observed tool inventory, and authorization.
The registry identity is the adapter's negotiated config key; it is not an
independent attestation of the remote server's implementation. Sources identify
compiled configuration, adapter-session observations, and unobservable facts.
The reporting process identity accompanies each row. A successful local probe
does not establish the daemon or remote worker's identity, credentials, sandbox,
or transport reachability.

External servers are only inspected through registry and inventory operations.
Even a connected server exposing every required tool reports
`authorization_unobservable`: no external tool is invoked. Tool names,
descriptions and `readOnly` hints do not establish mutation safety. The native
permission handler also refuses external tools, shell and file writes. An
external declaration using the reserved built-in name is refused before launch.

Authentication failures, denied tool access, transport failures, missing servers,
missing tools and alias mismatches have distinct categories. Unknown or absent
diagnostics remain unobservable. Custom launchers, other adapters and unsupported
platforms retain configured facts without claiming an observed connection.
The CLI does not resolve model or external MCP secrets: external servers with
credential references remain `credential-scope-unobservable` rather than using
an unrelated ambient credential. The disposable session does not copy stored
Copilot login files.

Operator-owned declarations for safely probing external tools are tracked in
[#6481](https://github.com/Agent-Clubhouse/Goobers/issues/6481). This report does
not infer or introduce that declaration contract.
