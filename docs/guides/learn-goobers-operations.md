# Learn Goobers: operate, harden, and extend an Instance

This chapter follows [Learn Goobers](learn-goobers.md) and
[workflow authoring](learn-workflow-authoring.md). It turns a validated tutorial
Instance into a supervised service and shows how to extend it without weakening
its safety boundaries.

## Tutorial defaults versus production defaults

| Tutorial | Production |
| --- | --- |
| Disposable Instance | Durable, backed-up Instance |
| Manual trigger | Bounded schedule, backlog, signal, or webhook trigger |
| Mock or disposable repository | Least-privilege provider identity |
| One run at a time | Explicit concurrency based on host and provider capacity |
| Foreground command | OS-supervised daemon with logs and restart policy |
| Local inspection | Telemetry retention, alerts, and recovery procedures |
| Embedded example workflow | Reviewed, versioned configuration changes |

Do not make a workflow autonomous merely because it validates. Prove the
manual path, constrain readiness and concurrency, and review every provider
mutation first.

## Validate the complete operating surface

Run structural validation after each coherent configuration change:

```sh
goobers validate <instance-path>
```

Before enabling autonomous execution, verify external repository and harness
dependencies:

```sh
goobers validate --check-harness --check-repos <instance-path>
```

Validation compiles workflow graphs and checks static contracts. External
checks prove that the current host can resolve the selected harnesses and
repositories. Neither command starts a run.

## Supervise the daemon

Run the Instance under the host's service manager instead of a long-lived
interactive terminal:

```sh
goobers up <instance-path>
```

Use the platform-specific unit in [Daemon supervision](supervision.md) for
systemd, launchd, or Windows Task Scheduler. The service account needs:

- read/write access to the Instance;
- access to configured repositories and workcopy roots;
- only the provider and model credentials required by declared capabilities;
- the same `PATH` and toolchain used during validation.

The daemon owns scheduling, claims, and run orchestration. The Portal is an
operator view, not the service supervisor:

```sh
goobers dashboard <instance-path>
```

## Apply configuration changes safely

Treat configuration as code:

1. edit a reviewed source or the durable Instance configuration;
2. run `goobers validate`;
3. inspect changed graphs with `goobers workflow show`;
4. apply the configuration using the documented reload or service-restart
   procedure for your deployment;
5. confirm that new runs pin the intended workflow version.

An in-flight run keeps its pinned compiled graph. A reload affects later runs;
it does not splice new YAML into an existing state machine.

## Bound concurrency and autonomous work

Start with one run:

```yaml
readiness:
  maxConcurrentRuns: 1
  maxRunsPerHour: 4

runControls:
  maxRepasses: 2
```

Increase limits only after measuring host capacity, provider rate limits,
workcopy disk use, and agent-harness concurrency. Keep claim/select work in a
deterministic first stage so two admitted runs cannot silently implement the
same item.

For scheduled workflows, use a bounded readiness policy and verify that the
first state can safely return no work. For agentic review loops, bound repasses
so disagreement escalates rather than consuming unbounded model and CI time.

## Manage credentials

Credentials are runtime inputs, never inline workflow data. Prefer the
platform credential mechanism documented by the relevant provider and
harness. Restrict each credential to the capabilities declared by its tasks.

Operational rules:

- never commit tokens, cookies, or browser profiles;
- use separate identities for read-only inspection and provider mutation when
  practical;
- validate repository and harness access under the service account;
- rotate credentials without editing workflow topology;
- restart or reload the supervised process after changing a credential source
  when the provider integration requires it;
- audit capability and policy-action additions as security-sensitive changes.

See [Secret stores](secret-stores.md) and
[Capability primitives](../reference/workflow-primitives/capabilities.md).

## Observe and debug runs

Use the journal as the source of truth:

```sh
goobers trace <run-id> <instance-path>
```

Correlate it with the compiled graph:

```sh
goobers workflow show <workflow> <instance-path>
goobers workflow show --dot <workflow> <instance-path>
```

Debug in this order:

1. identify the first failed or stalled graph node;
2. inspect that node's journal events, attempts, outputs, and artifacts;
3. distinguish compile-time configuration errors from runtime dependency
   failures;
4. repair the source declaration or external dependency;
5. validate before starting another run.

Do not edit scheduler state, run journals, or pinned graph files by hand.
Chapter 2 contains the full
[testing and debugging ladder](learn-workflow-authoring.md#11-test-at-the-smallest-useful-layer).

## Telemetry, retention, and capacity

Monitor:

- admitted, completed, aborted, and escalated runs;
- queue and stage duration;
- retries and repasses;
- provider, harness, and local-CI failures;
- workcopy and artifact disk consumption;
- daemon restarts and configuration reload failures.

Define retention for run artifacts, logs, and telemetry before autonomous
operation. Preserve enough history to correlate a failure with the compiled
workflow version and source commit.

## Back up or move an Instance

Stop the daemon before taking a consistent filesystem backup or moving the
Instance. Preserve the whole Instance rather than copying only `config/`; the
runtime state is required to retain run history, claims, schedules, and pinned
workflow versions.

After restoring or moving:

1. update host-specific paths and service definitions;
2. ensure credentials are available on the destination host;
3. run `goobers validate --check-harness --check-repos <instance-path>`;
4. inspect the daemon and Portal before re-enabling autonomous triggers.

Do not duplicate a live Instance and run both copies against the same backlog.

## Upgrade Goobers

Upgrade one Instance deliberately:

1. back up the Instance and record the current binary version;
2. read the target release notes and feature matrix;
3. install the new release beside the old binary where rollback is possible;
4. run validation with the new binary;
5. review normalized graphs and migration diagnostics;
6. restart the supervised daemon on the new binary;
7. retain the previous binary until new runs complete successfully.

Use [Release installation and verification](releases.md) and review
[schema migrations](schema-migrations.md) when the target release changes
stored data. A run already in progress remains bound to its pinned workflow
definition; avoid removing a binary or harness needed to finish it.

## Extend the Instance

### Add a workflow

Start with the release-matched template or copy an existing reviewed workflow
inside the same Gaggle. Change one graph concern at a time, validate, and
inspect the compiled graph. Use Chapter 2 and the
[workflow primitive reference](../reference/workflow-primitives/README.md).

### Add a Goober

Define a narrow persona with one harness, only the skills and tools it needs,
and a capability ceiling. Reference it from explicit workflow tasks or gates.
Validation rejects tasks that request authority the Goober does not grant.

### Add a deterministic stage

Prefer a small deterministic command for claims, CI, provider mutation,
artifact publication, and other mechanically verifiable work. Declare its
command, inputs, outputs, capabilities, policy actions, and graph transition
separately.

### Add a skill

Skills are instructions and procedures consumed by a harness; they do not grant
authority. Keep a skill release-matched with the CLI and DSL it describes, and
pair it with explicit Goober tools and capabilities.

### Add another Gaggle

Use a new Gaggle when repositories, backlog policy, workflow ownership, or
operating limits need an independent boundary. A Gaggle connects those
resources; it is not just a folder. Reuse an instance-shared Goober only when
the same persona and capability ceiling are appropriate across Gaggles.

After any extension:

```sh
goobers validate <instance-path>
goobers workflow show <workflow> <instance-path>
```

Test at the smallest layer that exercises the change, then promote the same
reviewed configuration into supervised operation.
