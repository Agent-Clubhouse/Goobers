# Harness prompt transport

Agentic stages can render prompts far larger than an operating-system command
line allows. On Windows, `CreateProcessW` caps the whole serialized command line
at 32,767 UTF-16 code units, and `cmd.exe` (which runs `.cmd`/`.bat` launchers
such as npm shims) caps it at 8,191 characters. Linux and macOS have `ARG_MAX`.
Paths, flags and quoting all count against those limits.

Goobers therefore never puts a rendered prompt in a harness command line. Every
subprocess CLI adapter sends the complete prompt through the process's standard
input. No configuration is needed and no caller opts in for large prompts.

| Adapter | Prompt channel | Launch flags | Verified with |
|---|---|---|---|
| Copilot (direct CLI) | stdin | `-p=` (empty value) selects non-interactive mode; the CLI then reads the prompt from stdin | Copilot CLI 1.0.95 |
| Copilot (controlled session) | session RPC | none; the headless server receives no prompt and no stdin | unchanged |
| Claude Code | stdin | `-p` with the default `--input-format text`; no positional prompt | Claude Code 2.1 |
| Codex | stdin | `codex exec ... -` | unchanged |

The same channel carries every turn of an invocation: the initial prompt, the
completion-repair prompt, and reviewer-gate prompts. A completion-repair turn
reuses the initial command line, so it continues the same session (`--session-id`
for Copilot, `--resume <id>` for Claude Code) and only its stdin differs.
`.goobers/prompt.md` in the workspace still records the rendered initial prompt
for debugging.

## Launchers

On Windows, a stdin-carrying command resolves to npm's `.cmd` shim or the native
executable, not to npm's PowerShell `.ps1` shim. That shim forwards stdin through
PowerShell's `$input` enumerator, which buffers, splits and re-encodes it. With no
prompt on the command line, the `cmd.exe` newline and quote problems that made
the PowerShell shim necessary cannot occur.

A custom launcher configured with `runner.harnessCommand` receives the prompt the
same way and must forward its standard input to the CLI unchanged (for example
`exec copilot "$@"`). There is no fallback to an argument-vector prompt: a
launcher that drops stdin produces an empty prompt, not a truncated one.

## Limits

Stdin transport removes the command-line limit; it does not raise the model's
context window. A prompt larger than the selected model accepts still fails at
the model, and is reported as that harness's error. Stdin delivery closes at
end of input and runs under the invocation's existing timeout and cancellation.

Large workflow state passed between stages is a separate concern and is not
changed by prompt transport.

## Live verification

The always-on tests use fixtures of 8,192, 32,768 and 131,072 ASCII characters,
plus multiline JSON, quotes and backslashes, a leading `---`, BMP and
supplementary Unicode, and CRLF/LF line endings. They check that argv stays under
the `cmd.exe` limit and that a real receiver process reads the exact bytes.

Opt-in tests run a signed-in CLI with a prompt larger than the `CreateProcessW`
limit. They force a completion-repair turn and check that it continues the same
session:

```sh
GOOBERS_COPILOT_LIVE_SMOKE=1 go test -tags integration ./internal/harness \
  -run '^TestIntegrationCopilotStdinPromptAndRepair$' -count=1 -v
GOOBERS_CLAUDE_LIVE_SMOKE=1 go test -tags integration ./internal/harness \
  -run '^TestIntegrationClaudeStdinPromptAndResumedRepair$' -count=1 -v
```

Run them against each new CLI version before you adopt it.
