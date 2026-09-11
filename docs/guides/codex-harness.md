# Codex harness

Goobers can run agentic stages through the OpenAI Codex CLI:

```yaml
spec:
  harness: codex
  model: auto
  harnessOptions:
    effort: high
  tools: []
```

Install the CLI and configure `agent:model` with an OpenAI API key before
starting the daemon:

```text
npm install -g @openai/codex
goobers validate --check-harness <instance-path>
```

`agent:model` credentials are passed as `CODEX_API_KEY` when they are OpenAI
API keys. Stored `codex login` credentials are deliberately not copied because
Codex's workspace-write sandbox permits commands to read files outside the
workspace. The private per-run `CODEX_HOME` contains configuration only, and
the runtime home and temporary directories are removed after the invocation.

The adapter runs `codex exec --json --ephemeral` with a workspace-write
sandbox, disabled command network access and web search, disabled hooks,
ignored exec-policy rules, and the workspace explicitly marked untrusted.
Repository `.codex` configuration is therefore not activated. Goobers'
platform sandbox, when configured, still wraps the process.

Declared `mcpServers` are written to the private run configuration. Local
server credentials are forwarded by environment-variable name, and remote
header credentials use Codex's environment-backed header configuration;
secret values are never written to `config.toml` or argv. Declared servers
are required, so initialization failure fails the stage rather than silently
removing tools. Model-generated shell commands receive an explicit environment
allowlist that excludes model and MCP credential variables.

## Tool-policy limitation

Codex CLI does not currently expose a general allowlist for built-in shell and
file tools. Goobers therefore fails closed when a Codex goober declares a
non-empty `tools` list instead of silently exposing omitted tools. Use
`tools: []` only when unrestricted Codex built-ins are acceptable within the
configured sandbox. MCP-specific allowlists remain supported by Codex, but a
separate MCP-only allowlist is not exposed by Goobers; any non-empty
`spec.tools` declaration is rejected.

Because the shipped quickstart templates declare `tools: [shell]`, they cannot
be seeded with `--harness codex`; author the Codex goober explicitly.

The default executable is `codex`. Override it when needed:

```yaml
runner:
  harnessCommand:
    codex: [path-to-wrapper, codex]
```

See the public Codex documentation for
[non-interactive execution](https://developers.openai.com/codex/noninteractive),
[authentication](https://developers.openai.com/codex/auth), and
[MCP configuration](https://developers.openai.com/codex/mcp).
