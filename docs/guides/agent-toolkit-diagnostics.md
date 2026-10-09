# Diagnose an instance with the Goobers agent toolkit

The Goobers agent toolkit ships read-only diagnostic skills that let an external
coding agent answer questions such as "why did the last run fail?" or "did that
issue get a merged pull request?" from durable Goobers evidence. This guide
shows where those skills come from, how to make them available to your agent,
and what a diagnose session looks like.

The toolkit runs in your own agent harness (for example GitHub Copilot, Claude,
or another `AGENTS.md` consumer). It is distinct from `Goober.Spec.skills`,
which configures agents that run inside a Goobers workflow. For the broader
toolkit, including DSL authoring, see
[Use the Goobers agent toolkit](dsl-authoring-skill.md).

## The diagnostic skills

| Skill | Role in a diagnosis |
| --- | --- |
| `goobers-environment-resolver` | Runs first. Identifies the exact `goobers` binary, its release and DSL support, the instance root, the config source, and the configured target repositories, and verifies a release-matched contract source. Ambiguous values are reported as unresolved rather than guessed. |
| `goobers-run-operator` | Answers read-only questions about runs, failures, reviewer repasses, escalations, claims, issues, and pull requests from bounded CLI and provider evidence, and cites run IDs, event sequences, and stable status or error codes for every conclusion. |

The same toolkit also contains `goobers-getting-started`, `goobers-dsl-author`,
and `goobers-workflow-upgrade`. Those skills author or upgrade configuration and
are not needed for diagnosis.

The run operator never changes the instance, a repository, or provider state:
it does not start or stop the daemon, run, retry, resume, rerun, or cancel a
run, clear a block, release a claim, or create, edit, comment on, close, or
merge an issue or pull request. One caveat applies: `goobers trace` opens the
telemetry rollup and may create or migrate `telemetry.db`. Trace a copy of an
instance whose on-disk state must stay byte-identical, such as a preserved
incident image.

## Where the skills are published

Every Goobers release publishes the toolkit in two forms, built from the same
source revision as the binary:

- embedded in the `goobers` binary, installed with `goobers agent-kit install`
  (recommended for diagnosis);
- as the `goobers-agent-toolkit_<version>.zip` release asset, verified by that
  release's `SHA256SUMS`.

`goobers agent-kit install` places the skills in the configuration repository
at:

```text
.goobers/agent-toolkit/
  instructions/goobers.md
  adapters/copilot.md | claude.md | agents.md
  skills/
    goobers-environment-resolver/SKILL.md
    goobers-run-operator/SKILL.md
    ...
```

Everything beneath `.goobers/agent-toolkit/` is product-owned and
version-matched to the release that installed it. Repository-root
`.github/copilot-instructions.md`, `CLAUDE.md`, and `AGENTS.md` stay user-owned.

## Prerequisites

- A `goobers` binary on `PATH`, or at a path you will give the agent. Use the
  same release that runs the instance; the skills prefer release-matched
  material over the default branch. Linux, macOS, and Windows are supported.
- A configuration repository that is a Git repository root. It holds the
  installed toolkit and is where you start the agent session.
- Local read access to the instance root you want to diagnose. The daemon does
  not need to be running; a stopped daemon is reported as liveness evidence,
  not as a failed run.
- An agent harness that reads repository instruction files: GitHub Copilot
  (`.github/copilot-instructions.md`), Claude (`CLAUDE.md`), or an `AGENTS.md`
  consumer.
- Optional, for current issue and pull request state: existing read-only
  provider access. For GitHub, an authenticated `gh` CLI (`gh auth status`);
  for Azure DevOps, the `az devops` extension signed in to the organization.
  Without it, the skill still reports the references a run recorded but marks
  their current state unknown. The skills never read token files, `.env`
  files, or secret stores.

## Enable the skills

From the configuration repository, install the toolkit bundled with your
`goobers` binary and add a reference for your harness:

```sh
goobers agent-kit install --harness copilot .
```

Use `--harness claude` for `CLAUDE.md` or `--harness generic` for `AGENTS.md`.
Run the command again with another harness to reference the same toolkit from
a second instruction file. Install appends a small delimited block to the
instruction file and never overwrites existing content:

```markdown
<!-- goobers:agent-toolkit:begin -->
Follow `.goobers/agent-toolkit/adapters/copilot.md` for Goobers-related work.
<!-- goobers:agent-toolkit:end -->
```

Confirm the installation matches the binary:

```sh
goobers agent-kit check .
```

A healthy installation reports `state: current`, `update available: no`, and
`none` for both modified and missing owned files. After upgrading Goobers, run
`goobers agent-kit update .` to review the diff and
`goobers agent-kit update --write .` to apply it. Commit
`.goobers/agent-toolkit/` and the instruction file so every collaborator's
agent sees the same skills.

The release archive can also be copied in by hand; see
[Manual archive installation](dsl-authoring-skill.md#manual-archive-installation).
A hand-copied `payload/` does not include the installed
`.goobers/agent-toolkit/manifest.json`, so `goobers agent-kit check` reports
`missing-manifest` and the environment resolver will not select that copy as a
verified contract source. `goobers agent-kit install` refuses to claim an
existing `.goobers/agent-toolkit/` that has no installed manifest, so to switch,
remove the hand-copied directory first and then run the install command above.

## Ask the agent to diagnose

Start an agent session in the configuration repository. The adapter tells the
agent to load the canonical skill bodies, so you can ask in plain English. Name
the skill and the instance path to remove ambiguity:

> Use the Goobers run operator skill to summarize recent runs, issues, and pull
> requests for the Goobers instance at `~/goobers/my-instance`.

Other useful prompts:

> Use `goobers-run-operator` to explain why run `<run-id>` in
> `~/goobers/my-instance` failed. Cite the causal event.

> Use `goobers-run-operator` to list escalated `implement` runs from the last
> 20 runs in `~/goobers/my-instance` and what each escalation is waiting on.

> Use `goobers-run-operator` to check whether the issue touched by run
> `<run-id>` now has a merged pull request.

## What the agent runs

The agent first reports the environment resolver's findings (binary, release,
instance, config source, and targets), then gathers a bounded evidence set with
read-only commands such as:

```sh
goobers version --json
goobers versions --json
goobers config show --json <instance-root>
goobers runs list --json --limit=20 <instance-root>
goobers trace --json <run-id> <instance-root>
goobers status --daemon <instance-root>
goobers escalations show --json <run-id> <instance-root>   # escalated runs only
goobers workflow show <workflow> <instance-root>
```

It reads aggregate, claim, and learned-block state (`stats`, `claims list`,
`blocked list`) only after `status --daemon` confirms a live daemon. Provider
reads use `GET` requests against the repository resolved from the instance
configuration, for example
`gh pr view <id> --repo <owner>/<repo> --json number,state,url,mergedAt`.

You can run the same commands yourself to check an answer.

## Example: diagnose a failed run

Suppose the newest run of the `demo` workflow failed while the daemon was
stopped. Asked "why did my latest run fail?", the run operator:

1. resolves the binary and instance and reports them;
2. runs `goobers runs list --json --limit=20 <instance-root>` and selects the
   newest run, stating the 20-run window;
3. runs `goobers trace --json <run-id> <instance-root>` and reads `phase`,
   `terminalCause`, and the ordered `events[]`;
4. runs `goobers status --daemon <instance-root>` and reports that the daemon
   is not running, separately from the run outcome.

A typical answer leads with the conclusion and cites its evidence:

```text
Run e4d9957f… (workflow demo, gaggle demo) failed in stage `curate` on
attempt 1/1.

- phase: failed; run.finished status failed (seq 7)
- terminalCause: code run_failed, causalEventSeq 6 — the executor rejected
  network mode "none" on this host
- daemon: not running (liveness only; it did not cause the failure)
- window: newest 20 runs, no filters

No issue or pull request was touched, so no provider reads were made.
```

Each claim carries a run ID, event sequence, and stable code, so you can
confirm it with `goobers trace <run-id> <instance-root>`. To repair the run,
change the configuration or environment yourself and start a new run; the
skill will not do it for you.

## Troubleshooting

- **The agent does not use the skills.** Check that the instruction file
  contains the `goobers:agent-toolkit` block, that `.goobers/agent-toolkit/`
  exists in the session's working repository, and that the prompt names the
  skill.
- **The resolver reports the release or contract source as unresolved.** Run
  `goobers agent-kit check .`. A modified, missing, or out-of-date toolkit is
  not used as authoritative; run `goobers agent-kit update .` and review the
  diff. If the check reports `missing-manifest`, remove the hand-copied
  `.goobers/agent-toolkit/` directory and run `goobers agent-kit install` from
  the binary that runs the instance.
- **The instance or config source is ambiguous.** Give the agent the exact
  instance root and, when needed, the `goobers` binary path.
- **Issue or pull request state is unknown.** Authenticate `gh` or
  `az devops` with read access to the configured repositories.
