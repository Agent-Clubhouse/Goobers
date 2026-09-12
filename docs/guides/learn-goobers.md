# Learn Goobers

This is the shortest path from a clean machine to understanding and operating
Goobers. Work through the chapters in order, or use the route table to jump to
the task you need.

| Goal | Start here |
| --- | --- |
| See a complete workflow without credentials | [Chapter 1: first success](#chapter-1-first-success-without-credentials) |
| Configure an existing repository | [Chapter 1: guided repository setup](#guided-repository-setup) |
| Understand Gaggles, graphs, and compilation | [Chapter 2: workflow authoring](learn-workflow-authoring.md) |
| Test or debug a workflow | [Chapter 2: workflow authoring](learn-workflow-authoring.md#11-test-at-the-smallest-useful-layer) |
| Run Goobers continuously and safely | [Chapter 3: operations](learn-goobers-operations.md) |
| Add workflows, Goobers, skills, or Gaggles | [Chapter 3: extension](learn-goobers-operations.md#extend-the-instance) |

The examples use `goobers` from `PATH`. If you are developing from this
repository, substitute `bin/goobers`.

## The mental model

A Goobers **Instance** is one durable directory containing the active
configuration and runtime state. Its configuration describes:

- **Gaggles**, which bind repositories, backlog policy, workflows, and workers;
- **Workflows**, which declare a graph of tasks, gates, transitions, and
  terminal outcomes;
- **Goobers**, which define agent personas, harnesses, skills, tools, and
  capability limits.

Goobers does not execute workflow YAML line by line. It parses and validates the
resources, normalizes the declarations, and compiles each workflow into a
deterministic state-machine graph. A run pins that compiled workflow version.
Tasks perform work, gates choose declared edges, and terminal targets complete,
abort, or escalate the run.

This separation matters:

- compile time proves that the declared machine is coherent;
- run time resolves credentials, repository state, provider data, and task
  results;
- agentic stages can reason within their declared authority, but graph
  transitions and provider mutations remain explicit and auditable.

Chapter 2 develops this model in detail and shows both text and DOT graph
inspection.

## Prerequisites

1. Install Goobers using the guide for
   [Windows](quickstart-windows.md), [macOS](quickstart-macos.md), or
   [Linux](quickstart-linux.md).
2. Confirm the binary is available:

   ```sh
   goobers --version
   ```

3. Keep Instances in durable storage, outside hosted-agent, Codespaces, and
   other ephemeral worktrees.

The first lab requires no provider credential, repository, model token, or
network write. Native Windows cannot enforce the demo's network isolation; use
the WSL 2 path in the Windows guide for the credential-free lab.

## Chapter 1: first success without credentials

Create and run the embedded, release-matched demo:

```sh
goobers init --demo ./demo-instance
goobers run demo ./demo-instance
```

Expected result:

- `init` creates one disposable Instance at `./demo-instance`;
- `run` prints a run ID and waits for a terminal result;
- the deterministic workflow reaches `complete`;
- no provider or model credential is used.

Verify the result using the run ID printed by `run`:

```sh
goobers trace <run-id> ./demo-instance
goobers workflow show demo ./demo-instance
goobers workflow show --dot demo ./demo-instance
```

The trace is the chronological execution record. The workflow views show the
compiled state graph: nodes are tasks and gates, edges are declared
transitions, and reserved terminal targets end the run.

Open the Portal in a second terminal:

```sh
goobers dashboard ./demo-instance
```

The Portal's execution graph should match `workflow show`; selecting a node
correlates graph state with journal events and artifacts.

### If the lab fails

| Symptom | Recovery |
| --- | --- |
| `goobers` is not found | Reopen the terminal after installation or add the install directory to `PATH`. |
| The target is rejected as ephemeral | Choose a durable directory; use `--allow-ephemeral` only when you control the workspace lifetime. |
| Native Windows reports that isolation is unavailable | Run the lab in WSL 2 as documented in the Windows guide. |
| Validation reports changed or missing files | Remove the disposable demo directory and rerun `init --demo`. |
| A run does not finish | Use `goobers trace <run-id> ./demo-instance` and continue with Chapter 2's debugging section. |

For the longer disposable GitHub issue-to-PR lab, continue with the
[Quickstart lab](quickstart.md). It is optional; guided setup is the normal
route for a real repository.

## Guided repository setup

From a clean machine, the interactive Getting Started wizard checks the
repository and host prerequisites, derives provider identity, default branch,
CI command, toolchain, and available authentication, then creates the smallest
safe configuration it can:

```sh
goobers init --guided
```

The command intentionally has no positional Instance path. Choose the target
repository in the browser. By default, the wizard creates one durable
neighboring Instance containing both active configuration and runtime state.
Use `--instance-path` only when you intentionally need another durable
location.

The wizard:

1. inspects the selected local Git repository without running its code;
2. explains missing prerequisites and choices it cannot safely derive;
3. adapts release-matched canonical workflow modules;
4. creates the neighboring Instance;
5. validates the configuration, repository access, and selected harnesses;
6. displays the exact validation command and links to the relevant next
   chapter.

It does **not** execute a workflow.

After setup, run the command printed by the wizard. The general form is:

```sh
goobers validate --check-harness --check-repos <instance-path>
```

Then inspect the generated workflow before running it:

```sh
goobers workflow show <workflow> <instance-path>
```

To have an agent tailor the generated Gaggle further, copy this prompt:

```text
Use the Goobers Getting Started skill to inspect my target repository and
current Goobers Instance, then customize the smallest safe Gaggle for this
repository. Derive the CI command, toolchain, default branch, and conventions;
ask only for choices that cannot be safely derived; validate the result without
starting a workflow.
```

For provider-specific credentials, repository metadata, manual setup, and
recovery, use [Onboard an arbitrary repository](arbitrary-repo-onboarding.md).

## Continue learning

1. [Author, compile, test, and debug workflows](learn-workflow-authoring.md)
   explains Gaggle structure, workflow primitives, graph compilation,
   capabilities, gates, repasses, test layers, and journal-driven debugging.
2. [Operate, harden, and extend Goobers](learn-goobers-operations.md) covers
   daemon supervision, reloads, credentials, concurrency, telemetry, backup,
   moves, upgrades, and adding product extensions.
3. Use the [workflow primitive reference](../reference/workflow-primitives/README.md)
   when authoring exact YAML.

