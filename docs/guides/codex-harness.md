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
## Experimental local ChatGPT authentication

An explicitly opt-in, trusted-local experiment can use the operator's existing
ChatGPT-authenticated Codex CLI session instead of materializing an API key:

```yaml
spec:
  harness: codex
  model: auto
  harnessOptions:
    auth: ambient-chatgpt
  capabilities:
    - agent:model
  tools: []
```

`agent:model` remains required as a semantic capability, but this mode does not
resolve or materialize its `CODEX_API_KEY` grant. It removes any inherited
`CODEX_API_KEY`, so the CLI cannot silently select API billing. ChatGPT login
uses the subscription's Codex limits; API-key mode uses separately billed OpenAI
API usage. Removing `harnessOptions.auth` restores the current API-key behavior.

Ambient mode is deliberately for a trusted, interactive local operator session,
not CI, services, shared runners, or untrusted hosts. Goobers runs `codex login
status` before the stage and requires it to say `Logged in using ChatGPT`; API
key login, logged-out, ambiguous, and failed status checks are rejected.

By default Goobers also passes `cli_auth_credentials_store = "keyring"` to both
the status check and `codex exec`. This verifies and requires the OS credential
store without reading credential contents. Configure Codex itself to use that
store:

```text
codex login status
codex -c 'cli_auth_credentials_store="keyring"' login status
```

If the second command confirms ChatGPT, the CLI can use the OS credential
keyring for this experiment. `auth.json` is file-backed and contains renewable
access tokens; treat it like a password. It is never read, copied, parsed,
logged, journaled, or placed in a Goobers artifact.

Only if an operator accepts that file-backed trust boundary, add the separate
opt-in:

```yaml
harnessOptions:
  auth: ambient-chatgpt
  allowFileBackedCredentials: true
```

This is not keyring isolation: a workspace-write Codex session runs under the
same trusted local user and can discover that user's normal home paths. Do not
enable it for code or prompts you would not trust with that local-user boundary.

The installed CLI does not expose credential-store mode in `login status`.
Goobers therefore determines keyring usability by making the CLI perform its
own status lookup with the supported keyring configuration. With the file flag
enabled, the normal CLI store is used; Goobers still never examines `auth.json`.

Ambient invocations retain the real operator-owned Codex home solely for CLI
authentication and token refresh. They use `--ignore-user-config` and
invocation-scoped `-c` MCP settings, so Goobers neither modifies the user's
persistent Codex configuration nor copies authentication into an ephemeral
home. API-key invocations retain their private, temporary `CODEX_HOME`.

Setup, validation, rollback, and troubleshooting:

```text
# Sign in interactively once, then verify the intended identity.
codex login
codex login status
codex -c 'cli_auth_credentials_store="keyring"' login status

# Validate the Goobers configuration and harness.
goobers validate --check-harness <instance-path>

# Optional explicitly gated read-only smoke (never part of normal tests).
$env:GOOBERS_CODEX_AMBIENT_SMOKE='1'
codex exec --json --sandbox read-only --skip-git-repo-check "Respond with exactly: subscription-auth-ok"

# Roll back to API-key mode: remove harnessOptions.auth (and the optional flag).
```

If validation reports an API-key login or no login, run `codex login` in the
same user session and re-check. If keyring validation fails but policy permits
file-backed credentials, use the explicit `allowFileBackedCredentials: true`
flag; otherwise leave ambient mode disabled. Do not copy `auth.json` to make a
remote host work—use the normal API-key mode or an approved non-interactive
identity instead.

An instance that also runs `copilot` and/or `claude-code` goobers gives
Codex its own `agent:model` grant with `harness: codex` on the
`credentialGrant`, so its `CODEX_API_KEY` is never confused with another
harness's secret; see "Mixed-harness instances" in
`docs/guides/github-token-scopes.md`.

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
