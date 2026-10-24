# Harness host-process safety

An agentic stage runs its harness CLI as the operator's own user. Without a
sandbox that isolates process IDs, a shell command the agent issues can signal
any process that user owns: terminals, the browser, the agent host, the
Goobers daemon itself. A kill by PID goes only where the agent points it. A
kill by *name* can reach much further.

BSD/macOS `pkill` makes this easy to get wrong. It stops parsing options at
its first operand, so `pkill -f "cat" -U $(id -u) -n` treats `-U`, the uid and
`-n` as extra patterns, not flags. `-f "cat"` then matches every process whose
full command line contains `cat`, which includes every `/Applications/...`
path. A validation soak saw a claude-code implementer run exactly that command
to stop a stray `cat`. It sent SIGTERM to the operator's terminal, browser and
agent host. The same lane did it twice in one day ([#6302](https://github.com/Agent-Clubhouse/Goobers/issues/6302)).

This page lists what each harness does about name-based kills.

## claude-code: built-in deny

Every claude-code invocation carries a Claude Code permission deny for
name-based kills:

```text
--disallowedTools Bash(pkill:*) Bash(killall:*)
                  Bash(/usr/bin/pkill:*) Bash(/usr/bin/killall:*)
                  Bash(/bin/pkill:*) Bash(/bin/killall:*)
                  Bash(/usr/bin/env pkill:*) Bash(/usr/bin/env killall:*)
```

Both permission shapes carry it: `--permission-mode bypassPermissions` (a
goober with no `tools` list) and the `--tools`/`--allowedTools` allowlist (a
goober that declares `tools`). The same-session completion repair, sandboxed
runs, and every `runner.harnessCommand` override carry it too. Claude Code
evaluates deny rules before the permission mode, so it refuses the command
even under `bypassPermissions`.

Claude Code splits compound commands and looks through a fixed set of plain
wrappers. So the bare-name rules also deny:

- `echo a; pkill ...` and `true && pkill ...`
- `env pkill`, `sudo pkill` and `FOO=1 pkill`
- `command`, `nohup`, `timeout`, `nice`, `time` and `exec` in front of `pkill`
- `(pkill ...)` and `{ pkill ...; }`
- `... | xargs pkill`

An absolute path counts as a different command name. That is why
`/usr/bin/env` and the absolute `pkill`/`killall` paths have rules of their
own.

### PreToolUse hook and version compatibility

On Claude Code 2.1.302 or newer, Goobers also installs a `PreToolUse` hook for
`Bash` in the fresh per-run `CLAUDE_CONFIG_DIR/settings.json`. It blocks
`pkill` or `killall` anywhere in command text, including `bash -c`, `eval`,
`find -exec`, `xargs`, command substitution, and alternate executable paths.
Commands combining `pgrep` and `kill` are blocked too. Quoted documentation
containing these words can also be refused by this conservative check.

The hook invokes the Goobers executable directly with an argument vector,
without a shell or interpreter dependency. It has a ten-second timeout and
blocks on execution errors. Safe commands return no permission decision, so
the normal permission policy remains in force. The same settings apply under
`bypassPermissions`, a tool allowlist, and completion repair.
[Claude's hook reference](https://code.claude.com/docs/en/hooks) describes
exec-form hooks, `onFailure`, and `PreToolUse` denial precedence.

Older or unrecognized versions retain the existing `--disallowedTools`
denies and receive a preflight WARNING recommending an upgrade. They do not
receive an unsupported hook. Set the goober's
`harnessOptions.requireHostProcessHook: true` to require the hook: preflight
and invocation then refuse an older or unknown version. Embedded adapter
callers must pass the preflight version in `RunRequest.HarnessVersion` and
supply `SelfBin` when the host executable is not the Goobers CLI.

### Settings that cannot disable protection

Unattended runs load only the isolated `user` settings source. Goobers refuses
`--bare`, `--safe-mode`, `--restricted`, and any `--setting-sources` value
other than `user`. It also refuses hook-disabling environment switches.
These checks apply on legacy versions too.

Safe inline JSON `--settings` remains supported, including additional
permission denies and allows. Settings files are refused because their
contents can change after inspection. Inline settings cannot contain `hooks`,
`disableAllHooks`, `allowManagedHooksOnly`, or `env`; those keys could replace
or disable protection. Existing settings-file overrides must be converted to
safe inline JSON. Project/local settings do not override the isolated hook.

### Validation and limitations

Always-on fixtures cover command wrappers, malformed input, safe commands,
settings bypasses, both permission modes, and completion repair. The opt-in
hook probe runs one short model session using harmless PATH stubs and requires
a positive control, absent forbidden markers, and the CLI's actual hook denial:

```sh
GOOBERS_CLAUDE_LIVE_SMOKE=1 GOOBERS_CLAUDE_LIVE_SELF_BIN=/path/to/goobers \
  go test -race -timeout 30m -tags integration ./internal/harness \
  -run '^TestIntegrationClaudeBlocksWrappedNameKillHook$' -count=1 -v
```

The hook is defense in depth. Script contents, constructed command names,
direct signal APIs, arbitrary PID kills, and Windows process commands are not
comprehensively covered. A stage able to modify its own settings is not
contained by a hook. Older versions' prefix denies additionally miss literal
shell wrappers. Only OS signal/PID confinement closes the process-safety
class; runner cleanup must target only process groups it owns. Goobers' Linux
sandbox uses a PID namespace. Goobers' macOS sandbox denies signals except to
processes in the same sandbox. A stage can still stop its own children, but
cannot signal host processes outside its sandbox. This applies to every
sandboxed harness and deterministic shell stage; the runner's process-group
teardown runs outside the sandbox and remains available. Unsandboxed stages
still need the harness-specific mitigations described here.

## codex

Goobers always runs `codex exec --sandbox workspace-write`. On macOS, Codex's
own Seatbelt sandbox refuses to signal processes outside the sandbox. A
`kill -TERM` from inside it at a host process fails with `Operation not
permitted`, whether the target was found by PID or by name. Codex therefore
needs no deny list on macOS. Nobody has checked Codex's Linux sandbox for the
same property yet ([#7396](https://github.com/Agent-Clubhouse/Goobers/issues/7396)).

Goobers passes `--ignore-rules`, so Codex execpolicy `.rules` files (user or
project) are not loaded. A `.rules` deny for `pkill` would have no effect.

## copilot

Every Copilot stage carries `--deny-tool` rules for `shell(pkill)` and
`shell(killall)`, their `/usr/bin` and `/bin` forms, and `/usr/bin/env` wrappers.
The adapter adds them after configured arguments, including custom launchers,
tool allowlists, completion repairs, and fallback authentication prompts.
[Copilot's permission rules](https://docs.github.com/en/copilot/how-tos/copilot-cli/use-copilot-cli/allowing-tools)
keep denies authoritative alongside `--allow-all-tools`. The invocation
record includes the effective denies.

This is the enforced CLI and control-session mitigation for
[#7394](https://github.com/Agent-Clubhouse/Goobers/issues/7394). The CLI deny
arguments and host permission callback apply independently of the model's
system-prompt instructions.

Controlled SDK sessions also inspect `fullCommandText` before approving a
shell permission. They refuse a missing command, `pkill` or `killall` anywhere
in the text, and commands combining `pgrep` with `kill`. That covers literal
`bash -c`, `eval`, `find -exec`, and `xargs` wrappers. This conservative text
check may also refuse quoted documentation containing those command names.

An opt-in integration probe executes harmless PATH stubs through the actual
CLI's tool-authorization RPC without a model turn. It requires a successful
control command, a native refusal record, and absent forbidden-stub markers:

```sh
GOOBERS_COPILOT_HOSTSAFETY=1 go test -race -timeout 30m -tags integration \
  ./internal/harness -run '^TestIntegrationCopilotDeniesNameBasedKill$' -v
```

Copilot CLI 1.0.98 also refuses bare name-kills inside its shell tool, reporting
`kill_command_blocked`. Goobers retains its own policy for older versions and
wrapped commands.

These checks are defense in depth. Non-controlled CLI sessions rely on the
CLI's permission matcher. Script files, constructed command names, direct
process-signaling APIs, Windows process commands, and arbitrary PID kills are
not comprehensively covered. Use OS sandbox confinement for host isolation;
runner cleanup must target only process groups it owns.
