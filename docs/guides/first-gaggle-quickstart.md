# Your first gaggle on a non-Goobers repo

This quickstart walks through the minimum steps to get Goobers running against
a repository you own and watch a complete claim → implement → PR cycle from a
fresh `goobers init`. It is intentionally short; the full production runbook is
[arbitrary-repo-onboarding.md](arbitrary-repo-onboarding.md).

**Prerequisites:**

- `goobers`, `git`, `gh`, and the GitHub Copilot CLI on your `PATH`.
- A GitHub repository you own, with Issues enabled.
- A `make ci` (or equivalent) command that runs the project's build, lint, and
  tests non-interactively.
- A fine-grained GitHub PAT for the repository (Contents, Issues, Pull
  Requests: read and write; Checks: read-only).
- A fine-grained GitHub PAT for Copilot (Copilot Requests: read-only).

---

## 1. Export settings

```sh
export GOOBERS_INSTANCE="$HOME/my-goobers"
export GOOBERS_CONFIG_SOURCE="$HOME/my-goobers-config"
export GOOBERS_TARGET="your-org/your-repo"
export GOOBERS_GITHUB_TOKEN=github_pat_...
export GOOBERS_COPILOT_TOKEN=github_pat_...
```

Never put token values in YAML files.

Sign in with the Copilot harness once:

```sh
copilot login
```

## 2. Initialize

```sh
goobers init --guided "$GOOBERS_INSTANCE"
```

At the prompts:

- Layout: **new-local**, pointing at `$GOOBERS_CONFIG_SOURCE`.
- Template: **implementation** (include backlog-curation when you want the full
  curation loop; skip it for now to keep the first run simple).
- Target repository: enter `$GOOBERS_TARGET`.
- Credentials: enter the variable names `GOOBERS_GITHUB_TOKEN` and
  `GOOBERS_COPILOT_TOKEN` — not the token values.
- Agent toolkit: choose **copilot** to install the release-matched skills.

After init, the config source has this shape:

```text
$GOOBERS_CONFIG_SOURCE/
  instance.yaml.example
  manifest.yaml
  gaggles/
    example/
      gaggle.yaml
      goobers/
        coder/
          goober.yaml
          instructions.md
      workflows/
        default-implement.yaml
```

The starter workflow is
[`default-implement`](../../internal/instance/starter/gaggles/example/workflows/default-implement.yaml):
a manual-only, single-task loop that claims one `goobers`-labeled issue and
invokes the coder goober.

## 3. Point the gaggle at your repository

Open `$GOOBERS_CONFIG_SOURCE/gaggles/example/gaggle.yaml` and replace the
placeholder values:

```yaml
spec:
  project:
    owner: your-org         # ← your GitHub organization or username
    name: your-repo         # ← your repository name
    branch: main
    connectionRef: repo-token
  backlog:
    project: your-org/your-repo
    labels:
      - goobers             # ← the trust label you will create in step 5
    connectionRef: repo-token
```

If your project is not a Go project, add a `ciCommand` to override the
Go-default `["make", "ci"]`:

```yaml
spec:
  ciCommand: ["npm", "run", "ci"]   # or ["python", "-m", "pytest"], etc.
```

## 4. Review the starter workflow

Open
`$GOOBERS_CONFIG_SOURCE/gaggles/example/workflows/default-implement.yaml`.
The `query-backlog` task uses `trustLabel: "goobers"` — this is the
maintainer-applied label that guards what the coder is allowed to act on
(SEC-047). Do not rename it without also updating step 5.

The `implement` task invokes the `coder` goober. The starter coder opens a PR
itself (`tools: [github, shell]`). For a hardened production workflow, replace
the starter with the split implementation from
[`config-examples/gaggles/acme-web/workflows/implementation.yaml`](../../config-examples/gaggles/acme-web/workflows/implementation.yaml),
which uses a separate deterministic `push-branch` + `open-pr` stage so the
agentic session never holds PR credentials.

## 5. Create the trust label

```sh
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh label create "goobers" \
  --repo "$GOOBERS_TARGET" \
  --color "0E8A16" \
  --description "Maintainer-approved; eligible for agentic work" \
  --force
```

Only maintainers should apply this label. The workflow's trust gate is only as
strong as the label's access control on the target repository.

## 6. Validate

```sh
goobers validate --source-tree "$GOOBERS_CONFIG_SOURCE"
goobers config materialize "$GOOBERS_INSTANCE"
goobers validate "$GOOBERS_INSTANCE"
goobers validate --check-harness "$GOOBERS_INSTANCE"
```

Fix every error before continuing. Common mistakes at this stage:

- A `connectionRef` that doesn't resolve to a `manifest.yaml` connection name.
- A `spec.gaggle` value in `goober.yaml` or `workflow.yaml` still reading
  `example` when you renamed the gaggle.
- A missing `ciCommand` for a non-Go project.

See [failure-mode-cookbook.md](failure-mode-cookbook.md) for a catalog of
error messages and fixes.

## 7. Create a test issue and apply the trust label

```sh
ISSUE_URL=$(
  GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue create \
    --repo "$GOOBERS_TARGET" \
    --title "Add a short example to the README" \
    --body "Add one sentence explaining the project's primary use case to README.md."
)
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue edit "$ISSUE_URL" \
  --add-label "goobers"
```

Keep the first issue small and reversible. The coder will implement it and open
a PR; you review and close the PR manually.

## 8. Run the workflow manually

```sh
goobers run default-implement "$GOOBERS_INSTANCE"
```

The command prints the run ID and a trace command:

```text
created run 0123456789abcdef (workflow=default-implement gaggle=example)
finished: phase=completed
inspect with: goobers trace 0123456789abcdef "$GOOBERS_INSTANCE"
```

## 9. Watch the trace

```sh
goobers trace 0123456789abcdef "$GOOBERS_INSTANCE"
```

A successful run shows events like:

```text
[1] run.started workflow=default-implement gaggle=example
[2] stage.started stage=query-backlog
[3] stage.finished stage=query-backlog status=success outputs={"claimed-item":"..."}
[4] stage.started stage=implement
...
[N] stage.finished stage=implement status=success
[N+1] run.finished phase=completed
```

If the run ends with `phase=escalated` or any stage shows `status=failure`,
read the trace with `--transcripts` to see what the coder reported:

```sh
goobers trace --transcripts 0123456789abcdef "$GOOBERS_INSTANCE"
```

See [failure-mode-cookbook.md](failure-mode-cookbook.md) for common failure
patterns and how to diagnose them.

## 10. Inspect the pull request

After a successful run, visit the PR the coder opened in your repository.
Review it as you would any PR, then close or merge it. Goobers does not merge
automatically at the starter tier — the human decides.

## Next steps

- **Enable scheduled curation**: add the
  [`backlog-curation`](../../config-examples/gaggles/acme-web/workflows/backlog-curation.yaml)
  workflow so the curator scopes and labels issues before the implementer claims
  them.
- **Upgrade to the split implementation workflow**: use the reference
  `implementation.yaml` from `config-examples/acme-web` for a hardened loop
  with a reviewer gate, local CI, and bounded repasses.
- **Start the daemon**: once the manual acceptance run works, run
  `goobers up "$GOOBERS_INSTANCE"` and let the scheduler dispatch runs on its
  cron cadence.
- **Add a second gaggle**: see the multi-gaggle section of
  [arbitrary-repo-onboarding.md](arbitrary-repo-onboarding.md).
- **Author a custom workflow**: see [workflow-authoring.md](workflow-authoring.md).
