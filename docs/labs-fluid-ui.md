# Labs: Fluid UI customizations

Fluid UI (#8338) lets each Portal user keep their own presentation
customization. This page documents the store and API (#8415), how the
Portal applies a customization (#8416), natural-language requests and their
lifecycle (#8417), the shipped workflow that turns a request into a
validated customization (#8418), and the aggregate promotion report
(#8419).

## Safety model

- A customization is a typed document, never code. Unknown fields are
  rejected, and the document is capped at 16 KB.
- Each user can read and write only their own document. The key is derived
  from the authenticated issuer and subject. Anonymous loopback dashboards
  share one `local` profile.
- Writes need only the `view` role, because they affect nobody else and never
  touch runs, gates, or configuration. Pod and worker principals are refused.
- Writes use the standard mutation transport checks: a JSON content type and
  a same-origin loopback `Origin`.

## Document (`portal.customization/v1`)

```json
{
  "version": "portal.customization/v1",
  "density": "compact",
  "pages": { "runs": { "hidden": ["cost"], "order": ["timeline", "gates"] } },
  "runList": { "sort": "failing-first", "phases": ["review"] },
  "savedViews": [{ "name": "My failing runs", "route": "#/runs?status=failed" }]
}
```

| Field | Rules |
|---|---|
| `density` | `comfortable` or `compact` |
| `pages` | At most 32 pages. Each page lists at most 64 `hidden` and 64 `order` panel IDs. |
| `runList.sort` | `newest`, `oldest`, or `failing-first` |
| `runList.phases` | At most 16 phase IDs |
| `savedViews` | At most 20 views. Names are unique and 1–64 characters. Routes must start with `#/` and may not contain whitespace, `<`, `>`, `"`, or `\`. |

IDs are lowercase slugs matching `^[a-z][a-z0-9-]{0,63}$` and may not repeat
within a list. The Portal ignores IDs it does not render.

## In the Portal

Open **Labs → Fluid UI** (`#/labs/fluid-ui`) to see your saved document and
edit it with structured controls. **Preview** applies the draft in the current
browser tab only; nothing is saved, and stopping the preview or reloading
returns to your applied customization. **Apply** saves it (`PUT`), and **Undo**
and **Reset** call the routes below.

The Portal loads your customization once per session and applies:

| Field | Effect |
|---|---|
| `density` | `compact` tightens page padding, section spacing, and list rows |
| `pages.overview` | Hides or orders the Overview panels `summary`, `attention`, `activity`, and `warnings`. Unlisted panels keep their standard order after the listed ones. |
| `runList.sort` | Orders the loaded runs on the Runs page. `failing-first` puts `failed`, `aborted`, and `escalated` runs first. |
| `runList.phases` | Shows only these phases (`running`, `completed`, `failed`, `aborted`, `escalated`) on the Runs page unless the URL sets a status filter |
| `savedViews` | Quick links in the sidebar and on the Fluid UI page |

Run-list sort and phases are defaults: they apply to the runs already loaded,
and you can dismiss either one for the current visit from the note above the
list.

The Portal never breaks a page because of a customization. If the version is
unknown, the document fails validation, or the server marks it `incompatible`,
the Portal shows the standard UI with a notice. Pages, panel IDs, and phases
that the Portal does not render are ignored, and the notice lists them.

The **Feedback** tab on the same page lists your natural-language requests
(see [Requests](#requests)) with a link to each request's run. **Your UX is
ready** appears only when a request reaches `ready_to_preview`; **Preview**
shows the candidate in this browser tab only, and **Apply** saves it through
the request. Applied requests can be **Reverted**. Failed and unsupported
requests show why and offer **Retry**.

## API

| Method and path | Effect |
|---|---|
| `GET /api/v1/labs/fluid-ui/customization` | Returns your state: `customization` (null means the standard UI), `revision`, `updatedAt`, `undoDepth`, and `incompatible` |
| `PUT /api/v1/labs/fluid-ui/customization` | Validates the document and saves it as your current customization |
| `DELETE /api/v1/labs/fluid-ui/customization` | Resets you to the standard UI. Undo can restore the previous customization. |
| `POST /api/v1/labs/fluid-ui/customization/undo` | Restores your previous revision. Returns 409 `nothing_to_undo` when no history remains. |

The server keeps your last 20 revisions for undo. If an invalid document
is rejected, the 400 error message starts with the offending field path, for
example `savedViews[0].route: ...`. If a stored document no longer validates
(for example, after a downgrade), `incompatible` is `true` and the Portal falls
back to the standard UI.

Documents are stored under `<instance root>/labs/fluid-ui/`, one file per user.
Each file name is a hash of the user key.

## Requests

A request is a plain-language description of the change you want, such as
"make my run list compact and put failing runs first". Each request starts one
run of the shipped `fluid-ui-customize` workflow, which proposes a
`portal.customization/v1` candidate. The candidate is never applied
automatically: you preview it and apply it yourself.

| Method and path | Effect |
|---|---|
| `GET /api/v1/labs/fluid-ui/requests` | Lists your requests, newest first: `{"requests": [...]}` |
| `POST /api/v1/labs/fluid-ui/requests` | Body `{"prompt": "..."}` (6 to 2000 bytes). Starts a workflow run and returns the request (202). |
| `GET /api/v1/labs/fluid-ui/requests/{id}` | Returns one of your requests |
| `POST /api/v1/labs/fluid-ui/requests/{id}/retry` | Starts a new run for a `failed` or `unsupported` request (at most 5 attempts) |
| `POST /api/v1/labs/fluid-ui/requests/{id}/apply` | Saves a `ready_to_preview` candidate as your customization through the same validated write as `PUT` |
| `POST /api/v1/labs/fluid-ui/requests/{id}/revert` | Restores the customization from before an `applied` request. Returns 409 `revert_stale` if your customization changed since. |

Write bodies for retry, apply, and revert must be empty or `{}`. A request has
one of these statuses:

| Status | Meaning |
|---|---|
| `queued` | The run is admitted but has not started |
| `implementing` | The run is in progress |
| `ready_to_preview` | The run finished with a candidate that passed validation |
| `applied` | You applied the candidate |
| `failed` | The run failed, or the candidate was still invalid after one retry. `errors` and `detail` say why. |
| `unsupported` | The request asks for something the customization surface cannot express. `unmetNeed` holds a short label such as `cost-charts`. |
| `reverted` | You reverted an applied request |

The status is derived from the request's actual run journal when it is read,
so the Portal never reports completion before the run has produced a
validated candidate. Requests are stored with the customization store under
`<instance root>/labs/fluid-ui/requests/`, one file per user (hashed like the
customization file), so they survive a daemon restart. You can see only your
own requests; any other ID returns 404. Each user may have at most 3 requests
in flight and keeps the 50 most recent.

Submitting and retrying start a workflow run, so they need a running daemon
(`goobers up`) with the `goobers` reference gaggle. The `goobers dashboard`
cannot start runs and returns 503 `triggers_unavailable` for them.

### The `fluid-ui-customize` workflow

The workflow ships in the `goobers` reference gaggle with the
`fluid-ui-designer` goober:

1. `load-request` (`goobers fluid-ui-request --load`) loads the ID of the
   request this run serves, your current customization, and a description of
   the customization surface and its limits. It never writes the request text.
2. `propose` (agentic, scratch workspace) writes either one complete
   `portal.customization/v1` document or
   `{"unsupported": true, "unmetNeed": "<short-slug>"}`. It writes JSON only,
   never code.
3. `validate-candidate` (`goobers fluid-ui-request --validate`) checks the
   candidate with the same validation as the store. An unsupported marker is
   accepted, and its label is normalized to a short lowercase slug of at most
   four words.
4. The `candidate-valid` gate sends invalid output back to `propose` once,
   with the validation errors. A second invalid candidate aborts the run, and
   the request becomes `failed`.

### Privacy

The request text is stored only in your private request file. A private run
binding, which names you by your hashed user key and never holds the text,
maps each attempt's run to your request and is kept after the run ends. The
text is not part of the trigger key or the run's outputs, and it never enters
any aggregate.

The agent reads the text while proposing a candidate, so anything it writes in
its own words can reflect the request. Exact-text scrubbing (below) cannot
catch escaped, truncated, or paraphrased forms, so access is restricted too.
For a `fluid-ui-customize` run, every user other than you and admins:

- gets `403 run_content_private` for the run's artifacts, transcripts, and
  persisted Backprop attribution record;
- sees run detail, events, and stage attempts with agent progress text,
  agent objectives, gate rationale, instruction addenda, reasons, error
  messages, and terminal-cause messages removed;
- sees run lists and run detail without the run's terminal reason, latest
  error message, or review rationale and findings, and with each blocker
  reduced to its code; and
- sees `/telemetry/errors` entries for the run with an empty message.

A `fluid-ui-customize` run with no binding (one not bound yet, or one whose
binding an earlier version removed when it finished) is treated as private to
admins. These responses are sent `Cache-Control: private, no-store`. The live
event stream carries only cursors, read-model names, and run and workflow
identifiers, never run content.

When a request ends
as `unsupported`, only its normalized unmet-need label is recorded for the
aggregate report; a label that echoes the request text is not recorded.

The text is also kept out of the run itself, so the protections above do not
have to catch every path. It never enters the run's inputs or the
`load-request` artifact. The daemon hands it to the
`propose` agent as a private input instead:

- When the agentic stage of a `fluid-ui-customize` run starts, the daemon looks
  up the request bound to the run. It registers the text, and each of its
  lines of 6 bytes or more, with that run's secret registry. This is the same
  exact-value scrubber that keeps credentials out of the journal. A run whose
  request cannot be found fails before the agent starts.
- It then writes the text to `.goobers/private/` in the stage's disposable
  workspace (mode 0600) and lists that file under "Private inputs" in the
  agent's prompt. The file is removed when the session ends. `.goobers/` is
  never published or captured for recovery.
- Every event, artifact, span and transcript the run records is scrubbed
  against the registry before it is written. If the agent quotes the request,
  the run's API responses, transcripts and journal files show `[REDACTED]`
  instead.

A request must be at least 6 bytes, the shortest value the scrubber redacts,
so every accepted request can be protected.

What remains visible to you, admins, and anyone with filesystem access to
the daemon host:

- **Paraphrases.** Redaction matches exact text. The agent is told never to
  quote, restate or summarize the request. If it paraphrases anyway, or
  quotes a fragment shorter than a whole line, that wording is not redacted.
- **The candidate and the unmet need.** The candidate the agent writes, such
  as saved-view names, and the normalized unmet-need label of an
  `unsupported` request are run artifacts and outputs. They describe the
  result, so they can reveal what was asked. A label that echoes the request
  text is not recorded in the aggregate.
- **The agent's working copy.** While the stage runs, the request file sits
  in the stage workspace on the daemon host. Anyone with filesystem access to
  the instance can read it there and in your request file.

Private inputs are wired only for the local daemon runner. The
`fluid-ui-customize` workflow already runs only there, because `load-request`
reads the daemon host's request store.

## Aggregate learning and promotion candidates (#8419)

Every time the store applies, revises, resets, or undoes a customization, it
appends one event to `<instance root>/labs/fluid-ui/events.jsonl`. The log
rotates to `events.1.jsonl` after 1 MiB, so at most two files are kept.

Each event records only:

- `kind`: `applied` (from the standard UI), `revised` (one customization
  replaced by another), `reverted` (back to the standard UI, or a reset or
  undo to a different shape), or `unsupported` (an unmet need).
- `shape` and `from`: which controls differ from the defaults after and
  before the change, for example `density:compact`, `runs.hidden`,
  `overview.order`, `runs.sort:failing-first`, `runs.phase`, or `savedViews`.
  Closed enum values are kept. Panel IDs, phase IDs, saved-view names, and
  routes are never recorded. Pages outside the Portal's own page list are
  recorded as `other`.
- `at`: the event time.
- `user`: a pseudonymous token, an HMAC-SHA256 of the user key under a random
  salt. The token is used only to count distinct users. The salt is created
  once in `events.salt` (mode 0600) and never leaves the instance. Issuers,
  subjects, and request text are never written.

### Ranking

The report replays the log and groups users by their shape. A user counts
once per shape, however often they revise it. A user is **sustained** on a
shape when their latest state still has that shape at least 7 days after
they first entered it. A user has **reverted** a shape when they left it with
a `reverted` event. Candidates are ordered by sustained users, then lowest
reversion rate, then users currently on the shape, then all users who ever
held it. Unmet needs (short slugs such as `dark-mode`, recorded by the request
workflow, #8418) are listed with distinct-user counts.

Candidates are advisory product proposals. They never change global defaults
or any user's customization; maintainers decide what to promote.

```text
goobers labs fluid-ui report [--json] [path]
```

`path` is the instance root (default `.`). The same report is served at
`GET /api/v1/labs/fluid-ui/report` to any principal with the `view` role.
The report holds only shapes and counts, never tokens. On instances with very
few users, a shape count of one can still reveal that some user chose that
shape, but never which user or what panel, phase, or view names they used.
