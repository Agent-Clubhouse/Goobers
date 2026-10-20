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

### Precedence with your own settings

The deny is additive in both directions:

- **Your settings are kept.** Goobers does not inject or rewrite `--settings`.
  A `--settings` file or JSON in `runner.harnessCommand` reaches the CLI
  unchanged. Claude Code takes a single `--settings` value, so injecting a
  second one would clobber yours or be clobbered by it. `--disallowedTools` is
  a separate rule source that the CLI combines with every settings layer.
- **Ours cannot be removed.** A deny from any source beats an allow from any
  source. A `permissions.allow` entry for `Bash(pkill:*)` in your settings,
  `--allowedTools Bash` or `bypassPermissions` does not re-enable it.
- **You can add more.** Put additional `permissions.deny` rules in
  `--settings`, or pass your own `--disallowedTools` in `runner.harnessCommand`.
  Repeated `--disallowedTools` occurrences accumulate.

Before this was built in, the recommended workaround was this
`runner.harnessCommand` override:

```yaml
runner:
  harnessCommand:
    claude-code:
    - claude
    - --settings
    - '{"permissions":{"deny":["Bash(pkill:*)","Bash(killall:*)"]}}'
```

You no longer need it. It does no harm, so you can remove it whenever it is
convenient.

### Check a new Claude Code version

An opt-in live test runs a signed-in Claude Code through the adapter on both
permission shapes. It confirms that the CLI reports the `pkill` as a
permission denial. The test signals nothing: `pkill` and `killall` are stubs
on `PATH` that only write marker files. Like production, it copies your real
Claude Code login into the run, from `~/.claude/.credentials.json` or the
macOS login Keychain. The always-on unit tests never read it.

```sh
GOOBERS_CLAUDE_LIVE_SMOKE=1 go test -tags integration ./internal/harness \
  -run '^TestIntegrationClaudeDeniesNameBasedKill$' -count=1 -v
```

### What the deny does not cover

This guards against a known mistake. It is not a sandbox. With Claude Code
2.1.292, these shapes still run:

- any `sh -c` or `bash -c` wrapper, even a literal one such as
  `bash -c 'pkill -f x'`
- `eval pkill ...`
- `xargs -I{}`, as in `pgrep -f x | xargs -I{} kill {}`
- `find ... -exec pkill ... \;`
- `$(command -v pkill) ...` and a relative `./pkill`
- an absolute path other than the ones listed above
- `kill $(pgrep -f ...)`, or a script file that kills by name
- Windows name-based kills: `taskkill /IM ...` and PowerShell
  `Stop-Process -Name`

[#7393](https://github.com/Agent-Clubhouse/Goobers/issues/7393) tracks a stronger guard: a `PreToolUse` hook that matches `pkill` or
`killall` anywhere in the command text.

Only a sandbox that scopes signals or process IDs closes all of these.
Goobers' Linux sandbox (bubblewrap) runs the harness in its own PID namespace,
so host processes are out of reach. Goobers' macOS sandbox restricts file
writes but does not scope signals, so a sandboxed claude-code stage on macOS
still depends on this deny ([#7395](https://github.com/Agent-Clubhouse/Goobers/issues/7395)).

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
