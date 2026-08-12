# Copilot hosted-runner authentication spike

> **Correction (2026-08-12, #1996): headless PAT authentication works.**
> The limitation recorded by the original 2026-07-28 spike was fixed by #1996.
> A clean-profile runner can pass the harness preflight and reach an agentic
> stage when `COPILOT_GITHUB_TOKEN` is present in the Goobers process
> environment. The observations below remain as a historical record of #1126.

This timeboxed spike asked whether the repository's
`COPILOT_GITHUB_TOKEN` secret can drive the real `CopilotAdapter`
non-interactively on a GitHub-hosted Linux runner. It did not add a workflow or
CI job.

## Fixed probe

The probe used one manual workflow with one agentic task, `model: auto`, and no
provider writes. The fixed goal was:

```text
Return a successful result whose outputs contain
sentinel=GOOBERS_GHCP_AUTH_SPIKE_V1. Make no repository changes.
```

The task declared `expectedOutputs: [sentinel]`, which declares the output name
but does not constrain its value. The target result envelope was:

```json
{"status":"success","outputs":{"sentinel":"GOOBERS_GHCP_AUTH_SPIKE_V1"},"summary":"fixed hosted-auth sentinel","metrics":{}}
```

The exact value and terminal run state were checked from `events.jsonl` with
the deterministic `jq` assertion below. No model-specific wording or output
interpretation was involved.

## Environment and observations

The intended hosted target was the repository's then-current `ubuntu-latest`
baseline: Ubuntu 24.04 LTS, linux/amd64. No hosted job was checked in or
dispatched because the clean-profile production preflight failed before Goobers
created a run. The failure was in shared Go code and the Copilot CLI invocation,
not an OS-specific branch.

The control was captured with:

| Component | Captured value |
|---|---|
| Host | macOS (Apple silicon) |
| Go | 1.26.5 |
| Copilot CLI | 1.0.75 |
| Goobers | a local source build |

The results were:

| Profile | Authentication | Terminal result | Completion parse | Journal evidence |
|---|---|---|---|---|
| Empty `HOME` and `COPILOT_HOME`, no ambient model token | Copilot CLI exited 1 with `No authentication information found` and directed the operator to login or set a supported token | The shipped `goobers run` failed its automatic harness preflight; no stage started | Not reached | No run ID or journal was created |
| Empty profile with an `agent:model` token ref configured | Identical preflight failure; the capability credential was not resolved for the probe | Identical failure before run creation | Not reached | Run-directory count remained unchanged |
| Operator profile with a stored Copilot CLI sign-in | Production auth probe succeeded | A run completed; the `echo` attempt succeeded | Passed; output was the exact sentinel | `stage.finished` seq 6 records the sentinel and `run.finished` seq 8 records `completed` |

The successful control also recorded a `copilot-cli.transcript` span. Its
contents are intentionally not reproduced because transcripts can contain
repository context.

## Original root cause

The Copilot CLI itself documents `COPILOT_GITHUB_TOKEN` as a supported
headless authentication source. At the time of the spike, the token could not
satisfy the Goobers startup path:

1. Production wires an auth probe equivalent to
   `copilot -p "Reply with exactly: ok" --allow-all-tools --available-tools=`.
2. `CopilotAdapter.Preflight` launched that probe with `baseEnv`, before any
   invocation or scoped credential set existed.
3. The default-deny base environment deliberately excluded
   `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, and `GITHUB_TOKEN`.
4. `agent:model` was resolved and injected as `COPILOT_GITHUB_TOKEN` only later,
   in `CopilotAdapter.Run`. A clean runner never reached that method.

The additional dependency at that time was therefore a persisted interactive
OAuth device-flow sign-in from `copilot login`, available through the runner
account's `HOME`/credential store. An ephemeral GitHub-hosted runner does not
have that profile.

#1996 changed the preflight to copy an ambient model token into only the
tool-disabled sign-in probe. It did not add the token to general
`runner.envPassthrough`, so live stages still receive model credentials only
through the `agent:model` capability boundary.

## Reproduction

Use a disposable instance containing this minimal goober and task:

```yaml
# goobers/echo/goober.yaml
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: echo
spec:
  gaggle: smoke
  role: echo
  instructions: instructions.md
  harness: copilot
  model: auto
  capabilities: [agent:model]
  tools: [shell]
  scaleFactor: 1
  workflows: [auth-spike]
```

```markdown
<!-- goobers/echo/instructions.md -->
# Echo

Do not inspect or modify repository files. Return `status: success` and set
`outputs.sentinel` to the exact sentinel in the task. Write the standard result
completion file and do nothing else.
```

```yaml
# workflows/auth-spike.yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: auth-spike
spec:
  gaggle: smoke
  triggers:
    - type: manual
  start: echo
  tasks:
    - name: echo
      type: agentic
      goober: echo
      goal: >-
        Return a successful result whose outputs contain
        sentinel=GOOBERS_GHCP_AUTH_SPIKE_V1. Make no repository changes.
      capabilities: [agent:model]
      expectedOutputs: [sentinel]
```

Configure the instance credential by reference, never by value:

```yaml
credentials:
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN
```

The instance placeholder below must point to a disposable instance already
created with `goobers init`. Its target repository and `smoke` gaggle must be
configured, the goober and workflow files above must be installed in its config
source, and the credential reference above must be present in `instance.yaml`.
The host must provide `copilot`, `go`, and `jq`; on GitHub-hosted runners,
`RUNNER_TEMP` is provided by the runner and `COPILOT_GITHUB_TOKEN` is the
repository secret supplied to the step.

Then run the clean-profile case. Replace the one marked instance path. The
commands disclose only versions and terminal state:

```bash
set -euo pipefail

# REQUIRED PLACEHOLDER: replace this value with the disposable instance root.
export GOOBERS_INSTANCE="/absolute/path/to/disposable-auth-spike-instance"
: "${COPILOT_GITHUB_TOKEN:?repository secret is required}"
: "${RUNNER_TEMP:?hosted-runner temporary directory is required}"

go build -o ./bin/goobers ./cmd/goobers
copilot --version

export GOOBERS_COPILOT_TOKEN="$COPILOT_GITHUB_TOKEN"
export HOME="$RUNNER_TEMP/goobers-ghcp-home"
export COPILOT_HOME="$HOME/.copilot"
mkdir -p "$COPILOT_HOME"

./bin/goobers validate --check-harness "$GOOBERS_INSTANCE"
./bin/goobers run auth-spike "$GOOBERS_INSTANCE"
```

Since #1996, `validate --check-harness` passes the sign-in probe and `run`
reaches the agentic stage when `COPILOT_GITHUB_TOKEN` is set as above. Without
an ambient token or stored CLI session, both paths still fail authentication
before creating a run.

For a rerun, capture the run ID printed by `goobers run` and verify the parsed
result and terminal journal state directly. This assertion fails if the output
is absent or differs by even one character; `expectedOutputs` alone does not
provide that value check:

```bash
set -euo pipefail

export GOOBERS_INSTANCE="/absolute/path/to/disposable-auth-spike-instance"
export RUN_ID="<run-id printed by goobers run>"
export SENTINEL="GOOBERS_GHCP_AUTH_SPIKE_V1"

RUN_DIR="$(find "$GOOBERS_INSTANCE/gaggles" -type d \
  -path "*/runs/$RUN_ID" -print -quit)"
test -n "$RUN_DIR"

jq -e -s --arg sentinel "$SENTINEL" '
  ([.[] | select(
    .type == "stage.finished"
    and .stage == "echo"
    and .status == "success"
    and .outputs.sentinel == $sentinel
  )] | length == 1)
  and
  ([.[] | select(
    .type == "run.finished"
    and .status == "completed"
  )] | length == 1)
' "$RUN_DIR/events.jsonl" >/dev/null
```

## Remaining constraint

The preflight has no invocation from which to resolve the configured
`agent:model` credential. It reads the token from the ambient Goobers process
environment instead. A `token: {file: ...}` reference alone can authenticate
the eventual agentic stage but cannot authenticate daemon startup; the same
value must also reach the process as `COPILOT_GITHUB_TOKEN`.

On Kubernetes, consume the synced Secret in both places: mount it for the file
token reference and expose the same Secret key as `COPILOT_GITHUB_TOKEN` with
`secretKeyRef`. A stored Copilot CLI sign-in remains a supported alternative,
not a requirement for headless PAT authentication.
