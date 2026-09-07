# Learn Goobers: operate, harden, and extend an instance

This is the canonical operations and extension chapter for **Learn Goobers**.
It follows the [quickstart tutorial](quickstart.md) and the
[workflow authoring tutorial](learn-workflow-authoring.md). The examples are
release-matched: use the `goobers` binary and documentation from the same
release, and validate the definitions that binary will run.

This chapter is a learning guide, not a shortcut around the focused runbooks.
Follow the links for complete command references and platform-specific
procedures. In particular, do not copy a tutorial workflow into production
without applying the production checklist in section 4.

## 1. The tutorial-to-production boundary

The tutorial is deliberately small and safe to understand:

| Tutorial choice | Production-safe change |
| --- | --- |
| Manual trigger and a disposable repository | A bounded schedule or event trigger, readiness filters, and a real repository |
| Broadly visible example configuration | Least-privilege capabilities, named policy actions, and repository-scoped credentials |
| Short local run | Explicit timeouts, retry budgets, concurrency limits, and an escalation route |
| One gaggle and one workflow | Separate gaggles when trust, credentials, repositories, or telemetry must not mix |
| Source checkout examples | A durable instance root outside ephemeral workspaces and a reviewed config source |

Start production configuration with
[Onboard an arbitrary repository](arbitrary-repo-onboarding.md), not by editing
the quickstart's placeholders. Keep definitions in YAML and Markdown under
version control; the instance runtime owns journals and telemetry, not desired
state. The [architecture](../ARCHITECTURE.md) is the authority when a guide and
an older example disagree.

## 2. Operate the daemon

### Lifecycle and supervision

Run a foreground daemon while learning:

```sh
goobers up /absolute/path/to/instance
goobers status --daemon /absolute/path/to/instance
goobers down /absolute/path/to/instance
```

For unattended tier 1 or tier 2 operation, install the native supervisor:

```sh
goobers service install /absolute/path/to/instance
goobers service status /absolute/path/to/instance
```

The [daemon supervision runbook](supervision.md) documents the shared graceful
shutdown contract and the native forms for systemd on Linux, launchd on macOS,
and Windows Service Control Manager. `stop` and `start` pause and resume an
existing registration; reinstall only when changing the stable host or
instance path. Run the daemon as the account that owns its configured
credentials, and put every deterministic-stage executable on the supervisor's
`PATH`, not only on an interactive shell's `PATH`.

Shutdown cancels admission, drains admitted work, and then exits. The default
drain is unbounded because an active agentic stage owns a live session and
worktree. Use `goobers up --drain-timeout <duration>` only when the supervisor
has a matching, slightly longer stop deadline. A second interrupt is the
force-exit escape hatch; treat the resulting dirty restart as an incident to
inspect rather than silently deleting runtime files.

### Reloads and upgrades

Configuration is read and validated before it becomes active. Apply a reviewed
change with:

```sh
goobers validate --strict /absolute/path/to/instance
goobers apply /absolute/path/to/instance
```

For a config source that should be watched, use the documented
`goobers up --watch-config` mode. A failed reload leaves the last valid
configuration active; it must not partially replace a gaggle. An already
running stage keeps the definition and capability set it was handed, while
later admissions use the new version. Inspect the exact CLI flags and reload
interval in the [CLI reference](../cli/README.md).

Use the supervised self-update path described in
[Releases & packaging](releases.md). It verifies a release candidate, validates
the configuration, records activation, retains the old binary, and rolls back
on failed health. Product upgrades do not require reinstalling a supervisor.
After an upgrade, check `goobers version`, validate the instance, inspect
daemon status, and run a harmless workflow or read-only preflight before
resuming autonomous work.

### Concurrency, runs, and telemetry

Concurrency is a safety setting, not merely a throughput setting. Bound
simultaneous runs and hourly admission in the workflow's `readiness` and
`runControls`; set each goober's scale deliberately; and use backlog labels,
assignees, and gaggle boundaries to prevent two runs from selecting the same
work. The scheduler's claim ledger is authoritative. Never remove claim,
journal, run, or workcopy files by hand.

Use the journal for what happened and telemetry for aggregate health:

```sh
goobers status --json /absolute/path/to/instance
goobers runs list --json --limit=50 /absolute/path/to/instance
goobers trace --summary <run-id> /absolute/path/to/instance
goobers telemetry stats /absolute/path/to/instance
goobers telemetry errors /absolute/path/to/instance
```

The [CLI reference](../cli/README.md) covers telemetry export, retention,
compaction, and orphan cleanup. Keep enough retention to investigate failures,
and send exported telemetry only to a store with the same access controls as
the instance. Run journals and artifacts can contain proprietary source and
prompts even when known credentials are redacted.

## 3. Harden credentials and state

Credentials are references in configuration, never inline values. Grant each
stage only the capabilities and policy actions it needs. Keep model access,
provider access, repository writes, and merge authority separate. A
`repo:push` grant does not imply pull-request merge authority, and a goober
cannot expand the permissions granted by its workflow task.

Use the [credential scope guide](github-token-scopes.md) for GitHub and the
[Azure DevOps authentication guide](ado-authentication.md) for ADO. For
secret-backed configuration, use the
[secret stores guide](secret-stores.md). Validate without exposing values:

```sh
goobers validate --strict /absolute/path/to/instance
goobers validate --check-harness --check-repos /absolute/path/to/instance
```

Keep the instance root and config repository outside ephemeral agent
workspaces. Use `repo-readonly` or `scratch` workspaces when a stage does not
need to modify a checkout, and keep network access and tools to the declared
minimum. Review the [workflow primitive capability reference](../reference/workflow-primitives/capabilities.md)
and [stage command reference](../reference/workflow-primitives/stage-commands.md)
before adding a capability or external command.

### Backups and moves

Take a **cold** snapshot: stop the daemon, wait for all runs to finish, and
copy the instance's durable state as one point-in-time set. Preserve
`instance.yaml`, active `config/`, run journals, scheduler ledgers, telemetry
database files and sidecars, and configured durable assets. Do not copy managed
`workcopies/`, service registrations, secret values, or harness sign-in state.

The [backup and restore contract](instance-restore-contract.md) classifies every
path and gives the verification commands. The
[move runbook](move-local-instance.md) adds destination-host checks, including
re-provisioning credentials and adapting absolute paths. Never run source and
destination daemons concurrently: independent claim ledgers can duplicate
provider effects.

## 4. Make a tutorial workflow production-safe

Before enabling autonomous work, check each of these:

1. Replace placeholders with the real repository identity and confirm the
   provider account and branch.
2. Use a maintainer-controlled trust signal before consuming untrusted backlog
   content.
3. Bound readiness, concurrency, retries, timeouts, budgets, and repasses.
4. Declare every capability and externally mutating policy action explicitly.
5. Separate agentic decisions from deterministic provider mutations.
6. Route every gate outcome, including timeout and infrastructure failure.
7. Use the narrowest workspace, tool, network, and credential boundary.
8. Test provider writes in a disposable repository and inspect a complete
   journal before enabling a schedule.
9. Configure cold backups, retention, alerting, and a restore rehearsal.
10. Pin the release and DSL version while runs are in flight.

Use `goobers workflow show --dot` to review the compiled graph, then use the
focused [workflow primitive reference](../reference/workflow-primitives/README.md)
to resolve validation errors. The [DSL authoring guide](dsl-authoring-skill.md)
and [workflow authoring tutorial](learn-workflow-authoring.md) explain how to
make the corresponding definition changes without hand-editing runtime state.

## 5. Extend the workforce

Definitions as code make extension explicit and reviewable. The usual order is
to add a reusable persona, place it in a workflow, validate the graph, and only
then grant provider authority.

### Add a Goober

Scaffold a goober in an existing gaggle, then edit its instructions, harness,
skills, tools, workspace posture, and capabilities:

```sh
goobers scaffold goober release-notes /path/to/instance/gaggles/example
goobers validate --strict /path/to/instance
```

Keep prompts focused on decisions and evidence. Put deterministic mutations in
named stages instead of asking an agent to invent shell commands. The
[Goober requirements](../requirements/goober.md) define isolation, scale,
credential, and telemetry expectations.

### Add a workflow or deterministic stage

Use the workflow scaffold, then add a task or gate using the DSL reference:

```sh
goobers scaffold workflow release /path/to/instance/gaggles/example
goobers validate --strict /path/to/instance
goobers workflow show release /path/to/instance
```

A deterministic stage is appropriate for a command whose inputs, outputs,
capabilities, and side effects can be declared. A gate evaluates a normalized
result and must branch every outcome. A workflow remains portable when its
orchestration is deterministic and stages communicate through declared
envelopes and artifact pointers. See the
[workflow architecture](../ARCHITECTURE.md#3-the-runner-seam) and the
[workflow primitives](../reference/workflow-primitives/README.md).

### Add a skill

Skills are release-matched instruction modules for a goober or an external
agent toolkit. Add a skill to the configured skill tree, reference it from the
goober, and validate from the same release that will run it. The
[DSL authoring skill guide](dsl-authoring-skill.md) shows the authoring
boundary; the [agent toolkit guide](../../agent-toolkit/README.md) explains
how product-owned skills are installed without replacing user-owned
`AGENTS.md` or `CLAUDE.md`.

Do not put credentials, authenticated browser profiles, or mutable runtime
state in a skill. Keep reusable guidance platform-neutral and link to the
platform runbooks for commands.

### Add another Gaggle

Scaffold a gaggle when a team, repository, trust posture, credential set, or
telemetry boundary needs independent ownership:

```sh
goobers scaffold gaggle docs /path/to/instance
goobers validate --strict /path/to/instance
```

Give it its own repository bindings, goobers, workflows, run journals, and
workcopy root as appropriate. Do not use a second gaggle to evade a capability
grant or to make two schedulers share a claim ledger. The
[multi-instance and repository placement guidance](instance-placement.md) and
[architecture](../ARCHITECTURE.md#1-one-system-three-deployment-tiers) describe
the boundaries that remain shared and those that must remain isolated.

## 6. Platform differences

The workflow model and journal semantics are shared, but host commands and
support posture are not:

| Platform | Native supervision | Important differences |
| --- | --- | --- |
| Linux | systemd user service | `systemctl --user`; native network isolation and the standard local-runner path |
| macOS | launchd user agent | `launchctl`; review the packaged drain timeout before relying on long-running stages |
| Windows | Windows Service | `sc.exe`; NTFS ACLs replace `chmod`, use short paths and long-path support, and native network isolation is unavailable |
| Windows + WSL 2 | Linux daemon inside WSL | Use the WSL 2 preflight for workflows requiring Linux isolation; credentials and tools must exist in the distro |

Read the [Linux](quickstart-linux.md), [macOS](quickstart-macos.md), and
[Windows](quickstart-windows.md) guides before translating command blocks.
Windows uses PowerShell and `.zip` release artifacts; Linux and macOS use the
Unix installer and archive forms. Do not assume a login shell's `PATH`, POSIX
signals, `chmod`, or a native isolation backend exists on every host.

## 7. Release and site integration

This file is the canonical source for this chapter. Do not copy its prose into
a second website page. Release packaging copies the repository's `docs/` tree
into the versioned documentation payload and writes `docs/RELEASE.md`, so the
chapter is available beside the release-matched binary and onboarding assets.
Installed documentation is versioned by release; read the chapter from the
same installed release as the command you run.

Goobers-Site synchronization should import
`docs/guides/learn-goobers.md` from the matching release payload and preserve
its relative links. A site page may add navigation or presentation metadata,
but its body should link to this canonical source rather than maintaining a
second copy. When changing headings or links, check both repository-relative
links and the packaged release path; keep references to `main` out of
release-matched command instructions.

For a release author, the relevant checks are:

```sh
goobers validate --strict /path/to/instance
goobers version
```

Then inspect the generated release documentation under the installed version
directory and confirm that this chapter, its focused guides, and the
release-matched onboarding template are present together. The release process
owns archive staging and link adaptation; the tutorial owns the content.
