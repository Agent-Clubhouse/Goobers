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
agent host. The same lane did it twice in one day (#6302).

This page lists what each harness does about name-based kills.

## claude-code: built-in deny

Every claude-code invocation carries a Claude Code permission deny for
name-based kills:

```text
--disallowedTools Bash(pkill:*) Bash(killall:*)
                  Bash(/usr/bin/pkill:*) Bash(/usr/bin/killall:*)
                  Bash(/bin/pkill:*) Bash(/bin/killall:*)
```

Both permission shapes carry it: `--permission-mode bypassPermissions` (a
goober with no `tools` list) and the `--tools`/`--allowedTools` allowlist (a
goober that declares `tools`). The same-session completion repair, sandboxed
runs, and every `runner.harnessCommand` override carry it too. Claude Code
evaluates deny rules before the permission mode, so it refuses the command
even under `bypassPermissions`. It also refuses a compound command that
contains one, such as `echo a; pkill -f x`, `env pkill ...` or
`... | xargs pkill`.

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
on `PATH` that only write marker files.

```sh
GOOBERS_CLAUDE_LIVE_SMOKE=1 go test -tags integration ./internal/harness \
  -run '^TestIntegrationClaudeDeniesNameBasedKill$' -count=1 -v
```

### What the deny does not cover

This guards against a known mistake. It is not a sandbox. Claude Code matches
a Bash rule on the command name as written, so these still get through:

- a kill by name built at runtime: `sh -c "$cmd"`, a script file, or
  `kill $(pgrep -f ...)`
- a `pkill` or `killall` reached by an absolute path not in the list above
- Windows name-based kills: `taskkill /IM ...` and PowerShell
  `Stop-Process -Name`

Only a sandbox that scopes signals or process IDs closes these. Goobers'
Linux sandbox (bubblewrap) runs the harness in its own PID namespace, so host
processes are out of reach. Goobers' macOS sandbox restricts file writes but
does not scope signals, so a sandboxed claude-code stage on macOS still
depends on this deny.

## codex

Goobers always runs `codex exec --sandbox workspace-write`. On macOS, Codex's
own Seatbelt sandbox refuses to signal processes outside the sandbox. A
`kill -TERM` from inside it at a host process fails with `Operation not
permitted`, whether the target was found by PID or by name. Codex therefore
needs no deny list on macOS. Its Linux sandbox has not been checked for the
same property.

Goobers passes `--ignore-rules`, so Codex execpolicy `.rules` files (user or
project) are not loaded. A `.rules` deny for `pkill` would have no effect.

## copilot

Goobers does not add a deny for Copilot CLI stages. The Copilot CLI's own
system prompt tells the model not to use `pkill`, `killall` or other
name-based kills. That is an instruction, not something the CLI enforces. A
`runner.harnessCommand` other than plain `copilot` must satisfy the
[launcher contract](harness-launcher-contract.md), so you cannot add flags
there the way you can for claude-code. Run Copilot stages that need host
isolation under the Linux sandbox.
