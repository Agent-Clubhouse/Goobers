---
name: goobers-run-operator
description: Answer read-only questions about Goobers runs, failures, repasses, escalations, claims, issues, and pull requests from bounded CLI and provider evidence.
---

# Goobers run operator

Answer operational questions without changing the instance, its repositories,
or provider state. Run `goobers-environment-resolver` first and use the exact
binary, instance root, config source, and target repository identities it
selects. If any of those remain ambiguous, say what is unresolved instead of
guessing.

## Safety boundary

This skill takes no action that changes a run, a work item, a repository, or
configuration. That is a boundary on *observable state an operator relies on*,
not a claim that every permitted command is free of side effects: `trace`
opens the telemetry rollup and can create or migrate `telemetry.db` (see
"Inspect each selected run" below). State that limit plainly if a user asks
whether these commands can alter the instance.

Do not start or stop the daemon, trigger, retry,
resume, rerun, cancel, or abort a run, clear a block, release a claim, repair or
redact a journal, update configuration, modify a repository, or mutate an issue
or pull request. In particular, never invoke `goobers run`, `goobers up`,
`goobers runs cancel`, `goobers runs resume`, `goobers runs rerun`,
`goobers blocked clear`, `goobers claims release`, `goobers journal redact`,
provider create/edit/comment/close/merge commands, or a provider request whose
HTTP method is not `GET`.

Use supported Goobers CLI reads rather than scanning `gaggles/*/runs`,
`events.jsonl`, `scheduler/`, or `telemetry.db` directly. Do not use daemon log
text as outcome evidence. Do not open transcripts or artifact bodies by
default; use a selected stage transcript only when the user explicitly asks
for transcript-level detail and the structured trace cannot answer the
question. Redact secret-shaped values and summarize rather than dumping raw
content.

## Select a bounded evidence set

1. If the user supplied a run ID, select only that run. Otherwise discover the
   newest runs with:

   ```text
   <goobers> runs list --json --limit=20 <instance-root>
   ```

   Add `--workflow=<name>` or `--phase=<phase>` when the question supplies that
   scope. The default recent window is 20 runs. State that window in the
   answer. Expand it with a larger explicit positive limit only when the user
   requests a wider search or the first window identifies a specific reason to
   expand. The CLI does not impose a maximum, so keep the selected window as
   small as the question allows. Never omit `--limit` and never trace every
   discovered run.

2. Inspect each selected run with:

   ```text
   <goobers> trace --json <run-id> <instance-root>
   ```

   This is an in-process read and works when the daemon is stopped. For
   daemon health, use `<goobers> status --daemon <instance-root>` as separate
   liveness evidence. A stopped daemon does not imply a failed workflow. While
   a daemon is live, use the same supported CLI reads; use `trace --json
   --follow` only when the user explicitly asks to watch a live run.

   `trace` is **not** a pure read of the instance. Alongside the journal it
   requests telemetry enrichment, and the offline reader opens the rollup
   through `rollup.Open`, which creates `telemetry.db` if absent and applies
   any pending forward migrations. The run's answer never depends on that
   enrichment — it is best-effort and a missing or unreadable rollup is not an
   error — but the call can still change on-disk telemetry state. Do not
   present `trace` to a user as an operation that cannot modify the instance,
   and do not run it against an instance whose telemetry state must stay
   byte-identical (a preserved incident image, a snapshot under forensic
   review). For those, copy the instance first and trace the copy.

3. Use narrow supporting reads only when they answer the question:

   - `goobers escalations show --json <run-id> <instance-root>` after selecting
     an escalated run;
   - `goobers workflow show <workflow> <instance-root>` for the configured
     graph.

   The current release's aggregate and scheduler-ledger readers may initialize
   or delegate through their backing stores. Use these only after `status
   --daemon` confirms a live daemon:

   - `goobers stats --json --since=24h <instance-root>` for bounded aggregates;
   - `goobers claims list --json --gaggle=<gaggle>
     --provider=<provider> <instance-root>` for a claim question;
   - `goobers blocked list --json <instance-root>` for a learned-block question.

   When the daemon is stopped, keep local reads bounded and non-administrative:
   use bounded `runs list`, selected-run `trace`, `escalations show`, and
   `workflow show`. Report aggregate, claim, or learned-block state unavailable
   rather than initializing or delegating an administrative store. Note that
   "non-administrative" is not the same as "leaves no trace" — `trace` may still
   create or migrate the telemetry rollup, as described above.

   `workflow show` proves only what is configured. A `gate.evaluated` or
   `stage.finished` event proves what executed. Do not describe a configured
   route as a transition the run took.

If the bounded window contains no matching run, report the filters and window;
do not treat absence as a no-work run or silently scan the whole instance.

## Build conclusions from durable evidence

Treat `trace --json` fields as the primary run evidence:

- `identity.runId`, workflow version/digest, gaggle, trigger, and `startedAt`
  identify the run;
- `phase` is execution health: `running`, `completed`, `failed`, `aborted`, or
  `escalated`;
- `outcome` is the separate business decision for a completed run;
- ordered `events[].seq`, `type`, `time`, stage/attempt, gate/verdict/target,
  status, error code, outputs, artifacts, and `externalRef` explain the path;
- `terminalCause.causalEventSeq`, an escalated gate event's `seq`, and
  `escalations show` field `cause.causalEventSeq` identify why a run stopped;
- `repasses` is a summary, while the ordered gate events prove which review
  actually requested and then passed a repass;
- `knownSchema: false`, a missing terminal event, a sequence gap, or
  contradictory evidence limits what can be concluded.

Prefer stable error codes, statuses, verdicts, digests, and reference
identities over human messages. Messages may explain a code but are not a
substitute for it. Cite content-digested artifacts by name and digest without
opening them unless their contents are essential to the question.

### Classify outcomes without collapsing them

- **First-pass success:** phase `completed`, the executed path shows successful
  work, and the relevant review has no needs-changes-to-pass cycle.
- **Reviewer repass:** cite the earlier `gate.evaluated` needs-changes verdict,
  its target, the later verdict, and their sequences. A retry attempt alone is
  not a reviewer repass.
- **Failed:** phase and terminal `run.finished` status are `failed`; explain the
  causal stage/gate, attempt, error code, and causal sequence when present.
- **Aborted:** phase and terminal status are `aborted`. Do not report it as a
  failure or no-work result.
- **Escalated:** classify from the **current lifecycle segment only** — the
  events at or after the last `run.resumed`, or from the beginning when the run
  was never resumed. Within that segment, phase is `escalated` or an executed
  gate event has `escalated: true`. A journal keeps its pre-resume events, so a
  gate that escalated before a resume is **history, not the current outcome**: a
  run that was resumed and then completed successfully is a first-pass success
  or reviewer repass, and reporting it as escalated is wrong. Report the earlier
  escalation separately as history, with its sequence, and say it was superseded
  by the resume.

  Do not send such a run to `escalations show`. That command reads the *current*
  phase and exits non-zero with `run <id> has phase <phase>, not escalated`,
  which is the command correctly refusing a stale classification rather than a
  defect to work around. Use it only once the current segment establishes
  escalation; use it for the structured selector,
  selected branch, repass count, terminal reason, and artifact timeline.
- **No-work:** a successful executed path records a decisive
  `stage.finished` status of `no-work` and then completes. This means the
  workflow correctly found nothing; it is not a failed run, an aborted run, a
  skipped scheduler tick, or merely an empty recent search.
- **PR-created:** require a successful creation-stage output (for example,
  `created: true`) together with a `ref.touched` PR `externalRef`. A touched PR
  may predate the run and does not by itself prove creation.
- **Merged:** require durable merge-stage evidence and confirm the provider PR
  record is merged. A completed run, a pass verdict, or a PR reference alone
  does not prove merge.

When execution health is `completed` but the terminal gate declined an action,
say both facts: the machinery succeeded and the business outcome declined the
action.

## Follow issue and PR references

Only perform a provider read after the selected run exposes an
`externalRef.provider`, `externalRef.kind`, and `externalRef.id`. Resolve the
repository from the selected instance and gaggle returned by the environment
resolver, never from the current directory, a repository-name suffix, or an
unverified URL.

**A gaggle has two independent targets, and `externalRef.kind` selects between
them.** `spec.project` is the code repository; `spec.backlog.project` is the
separate repository or project that scopes work items. They are frequently the
same, but nothing requires it:

- `kind` is an issue or work item → resolve against **`spec.backlog.project`**;
- `kind` is a pull request → resolve against **`spec.project`**.

An `externalRef` carries an id but no repository, so using one target for both
kinds will silently read a *different, unrelated* item that happens to share
the number — an answer that looks well-formed and cites a real URL while being
about the wrong thing. Never fall back to the other target to make a lookup
succeed. If the needed field is absent from the resolved configuration, or the
`kind` does not determine which applies, report the reference unresolved and
state which mapping was missing; do not guess.

For GitHub, pass the exact resolved repository for that kind:

```text
gh issue view <id> --repo <backlog-owner>/<backlog-repo> --json number,title,state,url,closedAt
gh pr view <id> --repo <code-owner>/<code-repo> --json number,title,state,url,mergedAt,closedAt
```

For Azure DevOps, use a structured GET and pass the exact resolved
organization, project, and repository route values. For example:

```text
az devops invoke --http-method GET --area wit --resource workItems \
  --route-parameters project=<backlog-project> id=<id> \
  --org <organization-url> --output json
az devops invoke --http-method GET --area git --resource pullRequests \
  --route-parameters project=<code-project> repositoryId=<repository-id> \
  pullRequestId=<id> --org <organization-url> --output json
```

The same split applies: work-item reads use the backlog project, PR reads use
the code project. For PR reads, `repositoryId` must be the resolver-selected
repository identity; never infer it from the PR URL, current directory, or
project name.

The journal records what the run touched at an event timestamp; the provider
read reports current state at query time. Cite both when state may have changed.
If the target mapping or provider read is unavailable, retain the recorded
reference as evidence but mark current issue/PR state unknown.

## Answer format

Lead with the direct answer, then give only the evidence needed to support it.
For each material claim cite:

- run ID;
- event sequence and timestamp, or the bounded run-list timestamp;
- workflow/gate/stage and stable status, verdict, or error code;
- issue/PR provider, ID, and verified link when provider evidence was read.

State the recent limit and filters for aggregate answers. Label inferences and
uncertainty explicitly. Never manufacture a missing transition, result,
reference, timestamp, or link.

The fixture and expected-evidence corpus at
`references/question-corpus.json` covers recent first-pass success, CI failure,
reviewer repass, escalation, scheduled no-work, abort, PR creation, merge,
cross-provider references, incomplete journals, stopped-daemon reads, and
live-daemon liveness.
