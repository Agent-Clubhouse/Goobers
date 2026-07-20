# Custom deterministic stage cookbook

Use this pattern when an existing script should run as a workflow stage and a
gate should branch on one of its numeric results. The runnable reference copy is
[`config-examples/gaggles/acme-web`](../../config-examples/gaggles/acme-web):

- `scripts/check-todos.sh` counts tracked TODO markers;
- `workflows/todo-check.yaml` runs it and branches on `todoCount`.

The stage runs in the configured project's fresh worktree. Put the script in the
**project repository**, not only in the Goobers instance's `config/` directory,
and commit it on the configured project branch.

## 1. Write the script

Create `scripts/check-todos.sh` in the project repository:

```sh
#!/bin/sh
set -eu

result_file=${GOOBERS_INPUT_RESULTFILE:-todo-check-result.json}
listing_file=${TMPDIR:-/tmp}/goobers-todos.$$
trap 'rm -f "$listing_file"' EXIT HUP INT TERM

if git grep -nI -e '[T]ODO' -- . >"$listing_file"; then
	:
else
	status=$?
	if [ "$status" -ne 1 ]; then
		exit "$status"
	fi
fi

todo_count=$(wc -l <"$listing_file" | tr -d '[:space:]')
cat "$listing_file"
printf '{"todoCount":%s}\n' "$todo_count" >"$result_file"
```

```sh
chmod +x scripts/check-todos.sh
git add scripts/check-todos.sh
git commit -m "chore: add TODO check"
git push
```

`git grep` exits `1` when it finds no matches, so the script converts that
expected condition into a successful check with `todoCount: 0`. Other grep
errors remain nonzero failures. The full listing goes to stdout; Goobers records
stdout as an artifact. The declared result file carries the small scalar that
the gate consumes.

## 2. Declare the stage and gate

Add this workflow under the gaggle's `workflows/` directory in the instance
config. Set `spec.gaggle` to the `metadata.name` from that gaggle's
`gaggle.yaml` (`example` is the name created by `goobers init`).

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: todo-check
spec:
  gaggle: example
  displayName: Custom TODO check
  triggers:
    - type: schedule
      schedule: "@weekly"
  readiness:
    maxConcurrentRuns: 1
  start: check-todos
  tasks:
    - name: check-todos
      type: deterministic
      goal: Count tracked TODO markers and publish the full listing.
      timeoutSeconds: 30
      run:
        command: ["sh", "scripts/check-todos.sh"]
        env:
          LC_ALL: C
      inputs:
        resultFile: "todo-check-result.json"
        maxOutputBytes: "65536"
      capabilities: []
      next: todos-found
    - name: report-todos
      type: deterministic
      goal: Report that TODO markers were found.
      run:
        command: ["sh", "-c", "printf '%s\n' 'TODO markers found; inspect the check-todos stdout artifact.'"]
      capabilities: []
    - name: report-clean
      type: deterministic
      goal: Report that no TODO markers were found.
      run:
        command: ["sh", "-c", "printf '%s\n' 'No TODO markers found.'"]
      capabilities: []
  gates:
    - name: todos-found
      evaluator: automated
      automated:
        check: output-numeric-gte
        params:
          key: todoCount
          threshold: "1"
      branches:
        pass: report-todos
        fail: report-clean
```

`output-numeric-gte` asks whether `todoCount >= 1`. Here `pass` means the
predicate is true and routes to `report-todos`; `fail` means the predicate is
false and routes to `report-clean`. A gate's `fail` outcome is a branch label,
not a failed workflow stage.

This script only reads the worktree and needs no external credential, so the
capability list is explicitly empty. Declare any capability the command really
uses; undeclared capabilities receive no credential and fail closed.

## 3. Understand `resultFile`

The script writes a **flat JSON object**, not a complete
`ResultEnvelope`. After the command exits, the shell executor:

1. derives the envelope `status` from execution;
2. records stdout, stderr, and the declared result file as artifact pointers;
3. merges the result file's top-level string, number, and boolean fields into
   `outputs`.

For two matches, the effective result has this shape (paths and digests
abbreviated):

```json
{
  "status": "success",
  "outputs": {"todoCount": 2},
  "artifacts": [
    {"path": "artifacts/...", "digest": "sha256:...", "mediaType": "text/plain"},
    {"path": "artifacts/...", "digest": "sha256:...", "mediaType": "text/plain"},
    {"path": "artifacts/...", "digest": "sha256:...", "mediaType": "application/json"}
  ]
}
```

The stdout pointer contains the full listing. Arrays, objects, and other bulk
data do not belong in `outputs`; write them to stdout/stderr or another
runner-supported artifact and pass the resulting pointer. Nested JSON such as
`{"outputs":{"todoCount":2}}` is not flattened.

### Exit code and status

- Exit `0` plus a present declared result file produces `status: success`.
- A nonzero exit produces `status: failure`, even if the result file exists.
- Exit `0` without the declared result file produces
  `missing_result_file`.
- Do not put `status` in the result file. It is executor-owned and a top-level
  string there would only become an output named `status`.

### When to emit no-work

A result file containing `"noWork": true` makes the executor return
`status: no-work`; the runner then completes the workflow without evaluating
the declared next state. Use it when a producer correctly found no subject for
downstream work, such as an empty backlog claim.

Do **not** emit no-work for this check. Zero TODOs is a meaningful result that
the `todos-found` gate must evaluate.

### Limits and environment

- `timeoutSeconds` bounds each task attempt; the default for deterministic
  commands is 10 minutes. A timeout kills the process group and returns a
  retryable failure.
- `limits.maxTokens` and `limits.maxCostUSD` carry optional agent-usage bounds
  into the invocation envelope. `limits.maxDurationSeconds` is also accepted,
  but `timeoutSeconds` takes precedence when both are set.
- `maxOutputBytes` is a positive decimal byte count for each captured stream.
  The default is 1 MiB per stdout/stderr stream. Excess output is truncated and
  reported by `stdoutTruncated` or `stderrTruncated`.
- The command's working directory is the fresh project worktree.
- Ambient environment is default-deny. Add explicit command variables under
  `run.env`. Goobers otherwise carries only tool/runtime
  basics (`PATH`, `HOME`, `TMPDIR`, XDG, locale, CA/proxy, and Go toolchain
  variables), declared inputs as normalized `GOOBERS_INPUT_*` variables, and
  credentials for declared capabilities as `GOOBERS_CRED_*`.
- A custom command does not receive `GOOBERS_RUN_ID`, `GOOBERS_WORKFLOW`, or
  `GOOBERS_INSTANCE_ROOT`; those operational variables are reserved for stages
  whose command is the `goobers` CLI. It also does not inherit arbitrary daemon
  variables or undeclared secrets.

## 4. Validate, run, and trace

From the Goobers repository, validate the instance and run the workflow:

```sh
goobers validate ./my-instance
goobers run todo-check ./my-instance
```

The run command prints the run ID and the exact trace command:

```text
created run 0123456789abcdef (workflow=todo-check gaggle=example)
finished: phase=completed
inspect with: goobers trace 0123456789abcdef ./my-instance
```

Run that command. The trace includes the scalar output, artifact records, and
the selected gate branch:

```text
[4] artifact.recorded name=.../stdout.log digest=sha256:... size=84
[7] stage.finished stage=check-todos attempt=1 class=policy status=success outputs={"todoCount":2}
[8] gate.evaluated gate=todos-found verdict=pass target=report-todos
```

To read the full listing from the human-readable journal, resolve the stdout
artifact path from the same run's `events.jsonl`:

```sh
RUN_ID=0123456789abcdef
RUN_DIR="./my-instance/runs/$RUN_ID"
ARTIFACT_PATH=$(
  jq -r 'select(.type == "artifact.recorded" and (.name | endswith("check-todos/stdout.log"))) | .ref.path' \
    "$RUN_DIR/events.jsonl"
)
cat "$RUN_DIR/$ARTIFACT_PATH"
```
