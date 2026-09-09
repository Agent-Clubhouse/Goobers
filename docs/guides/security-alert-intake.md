# Security alert intake

Goobers can read a repository's own security alerts as scheduled work-nomination
evidence: GitHub code-scanning alerts (CodeQL and any other tool uploading SARIF)
and Dependabot alerts. Both are strictly read-only surfaces, behind two separate
least-privilege capabilities, and the artifact they produce is untrusted by
construction.

- Issue [#2984](https://github.com/Agent-Clubhouse/Goobers/issues/2984) — code scanning
- Issue [#2987](https://github.com/Agent-Clubhouse/Goobers/issues/2987) — Dependabot

## What this is for, and what it is not

This intake exists for **default-branch alerts that nobody is working on**: an
open CodeQL finding on `main`, or a vulnerable dependency in a shipped manifest.
Those are durable backlog work, and before this they were invisible to
autonomous curation — a repository could carry eleven open CodeQL alerts while
the scheduled nomination workflow reported no candidate findings at all.

It is **not** for a CodeQL check failing on an already-open implementation PR.
That is ordinary CI, and the PR's own `ci-poll` / repass path already handles it.
Filing backlog work for it would duplicate a loop that is already running. This
is why the shipped example passes `--ref refs/heads/main`: the scope is the
default branch, deliberately.

## The two capabilities

| Capability | Fine-grained PAT permission | Reads |
| --- | --- | --- |
| `github:code-scanning:read` | Code scanning alerts: Read-only | `GET /repos/{owner}/{repo}/code-scanning/alerts` |
| `github:dependabot-alerts:read` | Dependabot alerts: Read-only | `GET /repos/{owner}/{repo}/dependabot/alerts` |

They are separate because GitHub's permissions are separate. A gaggle that wants
dependency intake but not static-analysis intake declares one stage and one
grant; neither credential is ever released to the other stage. Neither grant
authorizes an issue write, a pull-request write, or a contents write — the stage
that *files* a nomination is a different stage with its own `github:issues:write`
credential, and the `goobers:approved` trust gate is unchanged: a nominated issue
still lands unapproved for a maintainer.

A backend that does not implement the surface (Azure DevOps, Gitea) does not
declare the capability, so the stage is refused by name rather than returning an
empty alert list — which a nominator could not tell apart from "this repository
has no alerts".

## The stage

```yaml
- name: gather-code-scanning-alerts
  type: deterministic
  goal: Materialize open default-branch code-scanning alerts as bounded, untrusted nomination evidence.
  run:
    command: ["goobers", "security-alerts-query", "--source", "code-scanning",
              "--ref", "refs/heads/main", "--severity", "critical,high,medium",
              "--max-results", "50"]
  inputs:
    resultFile: "code-scanning-alerts.json"
  capabilities:
    - github:code-scanning:read
  continueOnError: true
  next: gather-dependabot-alerts
```

`continueOnError: true` is deliberate: a provider outage or a not-yet-granted
permission must not discard the telemetry nomination the same run already
gathered. The nominator sees the stage's typed error instead of an alert set.

Filters: `--state` (default `open`), `--severity`, `--max-results` (default 100,
ceiling 1000) apply to both feeds. `--tool` and `--ref` are code-scanning only;
`--ecosystem` and `--scope` are Dependabot only. Mixing them is a usage error,
caught before any credential is resolved.

## The artifact

`security-alerts-v1` (`api/schemas/security-alerts-v1.schema.json`). It records
the filters it applied, so it explains its own scope, and reports `truncated`
when the feed had at least `--max-results` alerts — a nominator must not read a
truncated set as a complete one and conclude an alert disappeared.

**Everything in it is untrusted.** Rule descriptions, analysis messages,
advisory summaries, file paths and package names are repository or third-party
content, so the artifact and every alert in it carry integrity `unapproved`. The
runner records it at that grade (the stage is a provider builtin, so its result
is graded from the artifact's own integrity labels), which means a consumer
declaring `minimumIntegrity: maintainer` will refuse it. The nominator that
reads it declares no minimum on purpose: reading untrusted evidence and
producing a proposal a maintainer approves is exactly its job. Alert text is
data to describe, never instructions to follow.

## Dedupe identity

Every alert carries a `dedupeKey`, and correlation must use it rather than the
alert number:

| Feed | Key | Why |
| --- | --- | --- |
| code scanning | `code-scanning:<tool>:<ruleId>:<path>` | One rule firing at twelve data-flow sites is one defect. Keying per location is how one scan becomes twelve near-duplicate issues. |
| Dependabot | `dependabot:<advisory>:<ecosystem>:<package>:<manifest>` | One advisory against one manifest is one nomination, however many times a scan re-reports it. |

The alert **number** is deliberately not identity: a repository re-scanned after
a rule upgrade can renumber its alerts, and a per-number key would file a fresh
issue for a defect already tracked.

A scheduled run should record the key in the body of any issue it files, then
comment the recurrence on that issue on later runs instead of filing a second
one. The shipped `config-examples` work-nomination workflow demonstrates the
whole shape.

## Related

- [GitHub token scopes](github-token-scopes.md) — the PAT permission behind each capability
- [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §5 — the capability model
