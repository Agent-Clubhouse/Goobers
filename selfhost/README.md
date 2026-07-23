# The Goobers self-hosting instance (`selfhost/`)

"Begins to build itself" (`docs/ARCHITECTURE.md` §12, the V0 definition of
done): this is the config-as-code that makes a Goobers instance target the
Goobers repo itself (`Agent-Clubhouse/Goobers`) — feed issues into the
backlog and watch them get curated, scoped, and implemented into PRs by the
instance running on your own machine.

This directory is the **config repo content** (`manifest.yaml` +
`gaggles/goobers/`) plus a template `instance.yaml.example` and this guide.
It is not itself an instance root — `goobers init` creates one, and you point
its `config/` at this directory's contents (see below).

## What's in here

```
selfhost/
  README.md               # this file
  instance.yaml.example    # template instance.yaml (no secrets — copy and use as-is)
  manifest.yaml             # top-level desired state (kind: Manifest)
  gaggles/
    goobers/
      gaggle.yaml            # the "goobers" gaggle: targets Agent-Clubhouse/Goobers
      goobers/
        curator/             # issues-only: dedupe, tag, split, mark ready
        implementer/          # repo:push only: issue -> code change
        reviewer/              # no write capability: adversarial code review
        nominator/              # issues-only + telemetry:read: files new issues from evidence
      workflows/
        backlog-curation.yaml    # #25 — 3x/day
        implementation.yaml       # #27 — 2x/day (the flagship loop)
        work-nomination.yaml       # #26 — 1x/day
```

## Guardrails (confirmed, not just described)

These are load-bearing on a public repo and are enforced by the checked-in
config itself, not left to operator discretion:

- **`goobers:approved` trust-label gate is ON** in both backlog-consuming
  workflows (`backlog-curation`, `implementation`) — backlog item text is
  untrusted input (`SEC-047`); only a maintainer-applied label makes an item
  claimable. Nothing a nominator files is pre-approved — it lands unclaimed.
- **The reviewer gate is ON** in `implementation` — every implementation run
  gets an independent, no-write-capability reviewer verdict (`pass` /
  `needs-changes` / `fail`) before a PR can reach the CI-poll/repass loop.
- **The instance never merges.** There is no merge stage anywhere in
  `implementation.yaml` — it ends at `close-out` once CI passes. Branch
  protection on `main` (the `make ci` GitHub Actions check, required,
  `enforce_admins=true`) plus a human review are the only path to `main`.
- **Budgets are low by default.** `implementation` fires twice daily
  (`readiness.maxConcurrentRuns: 1`, `maxRunsPerHour: 1`, cadence 2x/day) — a
  hard ceiling of exactly 2 runs/day, via cadence × hourly cap (the same
  pattern `config-examples/gaggles/acme-web` documents; `WorkflowSpec` also
  has a native `maxRunsPerDay` field, #340, as a more direct alternative).
  Curation and nomination run 3x/day and 1x/day respectively, each capped at
  `maxRunsPerHour: 1`.
- **Capability grants are minimal per goober** and checked at validate time
  (fail-closed on mismatch — a workflow task using a capability its goober
  doesn't grant is a validation **error**, not a warning): curator and
  nominator get `github:issues:write` only (+ nominator gets
  `telemetry:read`); implementer gets `repo:push` only; reviewer gets no
  capability at all.
- **The Tutor is confined to this config root.** When the self-tuning Tutor
  loop (#104 / epic #36) opens a config-as-code PR, its `open-pr` stage checks
  the run's git diff and refuses (fail-closed, no PR opened) to touch anything
  outside it — platform code, CI, and credentials are unreachable through it.
  Since Tutor PRs land in this same repo as platform code, `.github/CODEOWNERS`
  owns `/selfhost/` so a maintainer must review before merge. Structural
  credential scoping rides #35. See
  [`docs/guides/tutor-write-boundary.md`](../docs/guides/tutor-write-boundary.md).

## Tokens and scopes

You need a GitHub personal access token (fine-grained, scoped to
`Agent-Clubhouse/Goobers` only) with:

- **Issues:** read and write (curation, nomination, and the backlog-query
  stage all read/write issues).
- **Pull requests:** read and write (`open-pr`, `ci-poll`, review requests).
- **Contents:** read and write (the implementer pushes to a
  `goobers/<workflow>/<run-id>` branch — never `main`).
- **Checks / statuses:** read (CI-poll needs to see check results).

The `merge-review` workflow also needs `GOOBERS_GITHUB_REVIEW_TOKEN`, a
fine-grained PAT with **Pull requests: read and write** owned by a different
GitHub identity. GitHub does not allow the identity that authored a PR to
approve it, so `github:pr:review` cannot reuse the repo token for goober-authored
PRs.

Do **not** grant merge or admin permissions — the instance is never supposed
to merge, and branch protection should stay something only a human/repo
admin can touch.

## Setting up the instance

1. **Pick an instance root** — anywhere on your machine, e.g.
   `~/goobers-instance`. This is *your* choice; nothing in this repo assumes
   a specific path (the instance root is never checked in).

2. **Initialize it:**

   ```sh
   goobers init ~/goobers-instance
   ```

   This scaffolds `instance.yaml`, `config/` (seeded with a generic starter
   example), `runs/`, `scheduler/`, `workcopies/`, and a `telemetry.db`
   placeholder.

3. **Replace the seeded config with this one:**

   ```sh
   rm -rf ~/goobers-instance/config
   mkdir -p ~/goobers-instance/config
   cp -r selfhost/gaggles ~/goobers-instance/config/
   cp selfhost/manifest.yaml ~/goobers-instance/config/
   cp selfhost/instance.yaml.example ~/goobers-instance/instance.yaml
   ```

4. **Sign in and set repository tokens** (never inline them into
   `instance.yaml` — the loader rejects that, `CFG-009`/`SEC-010`):

   ```sh
   export GOOBERS_GITHUB_TOKEN=ghp_...
   export GOOBERS_GITHUB_REVIEW_TOKEN=github_pat_...
   copilot # sign in once; the local daemon reuses this stored session
   ```

   For a headless service or CI account, configure the commented
   `agent:model` entry in `instance.yaml` and set
   `GOOBERS_COPILOT_TOKEN` to a fine-grained PAT with Copilot Requests:
   Read-only.

5. **Validate before starting anything:**

   ```sh
   goobers validate ~/goobers-instance
   # OK: instance.yaml valid; config/ valid (1 gaggle(s), 4 goober(s), 3 workflow(s))
   ```

6. **Bootstrap the label taxonomy** on the target repo (idempotent — safe to
   re-run; `gh label create --force` creates or updates in place):

   ```sh
   for l in \
     "goobers:approved:0E8A16:Maintainer-approved — eligible for curation/implementation (SEC-047)" \
     "goobers:ready:1D76DB:Curated and scoped — eligible for implementation" \
     "goobers:claimed:FBCA04:Currently claimed by an in-flight run" \
     "goobers:nominated:5319E7:Filed by the nominator — awaiting maintainer approval" \
     "goobers:needs-human:D93F0B:Needs a decision only a human can make" \
   ; do
     IFS=: read -r ns name color desc <<<"$l"
     gh label create "$ns:$name" --color "$color" --description "$desc" \
       --repo Agent-Clubhouse/Goobers --force
   done
   ```

7. **Start the daemon** (scheduler + runner + telemetry rollup):

   ```sh
   goobers up ~/goobers-instance
   ```

   Collector push is optional. To inspect local traces in Jaeger, follow the
   [Jaeger quickstart](../docs/guides/jaeger-quickstart.md).

   > `goobers up`/`run`/`status`/`trace` land with issue #23 (CLI surface,
   > in progress). Until then, `init`/`validate` are enough to prepare and
   > check this config; `up` is where the daemon actually starts running
   > cycles against the live backlog.

## What to expect per cycle

- **Curation** (03:22 / 11:22 / 19:22 local): claims up to 20
  `goobers:approved` items with neither output marker, dedupes/tags/splits
  them, and marks each `goobers:ready` or `goobers:needs-human` with an
  explanatory comment.
- **Implementation** (08:17 / 20:17 local, ≤2/day): claims exactly one
  `goobers:approved` + `goobers:ready` item, implements it in an isolated
  worktree, passes it through the reviewer gate and a local `make ci` gate,
  opens a PR, polls CI with a bounded repass loop (repassing to the
  implementer on `needs-changes` or a CI failure), and comments on the
  originating issue once CI passes. It stops there — a human merges.
- **Nomination** (06:41 local, once daily): reviews telemetry/repo signals,
  checks the existing backlog for duplicates, and files well-evidenced
  issues carrying `goobers:nominated` + an evidence footer. Filed issues are
  **not** pre-approved — a maintainer reviews and applies `goobers:approved`
  before curation will touch them.

## Observing a run

```sh
goobers status                 # instance overview: workflows, next cron fires, active/recent runs
goobers trace <run-id>          # a run's full journal: timeline, stages, attempts, gate verdicts, artifacts
goobers trace --json <run-id>    # same, for scripting
```

Every scheduling decision and claim-ledger transition is also inspectable
directly with standard tools — the journal is human-readable first
(`docs/requirements/*`, `internal/journal/README.md`,
`internal/localscheduler/README.md`):

```sh
cat ~/goobers-instance/runs/<run-id>/events.jsonl | jq -c '{seq, type, stage, status}'
jq -c 'select(.type=="trigger.fired" or .type=="tick.skipped")' \
  ~/goobers-instance/scheduler/events.jsonl
```

Everything the instance did — every claim, trigger, run, stage attempt, gate
verdict, and artifact — is reconstructible from this journal alone; `goobers
trace` is a rendering of it, not a separate source of truth.

## Stopping safely

`goobers up` drains on `SIGTERM`: it finishes the current stage attempt,
checkpoints, and exits — `Ctrl-C` or `kill <pid>` is safe at any time. A
restart resumes any non-terminal run from its last completed stage (journal
replay) and picks the cron schedule back up from its last known fire (the
embedded scheduler's missed-tick policy collapses any downtime into at most
one catch-up run per workflow, never a backlog replay).

## Sensitive-info check

Nothing in this directory contains a token, a personal path, or a
machine-specific value — `instance.yaml.example`'s `token.env` is an
environment variable **name**, not a value, and the instance root itself is
never checked in (you choose it at `goobers init` time). Safe to keep this
directory in version control as-is.
