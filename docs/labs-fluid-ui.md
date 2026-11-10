# Labs: Fluid UI customizations

Fluid UI (#8338) lets each Portal user keep their own presentation
customization. This page documents the store and API (#8415), how the
Portal applies a customization (#8416), natural-language requests and their
lifecycle (#8417), and the shipped workflow that turns a request into a
validated customization (#8418).

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
| `POST /api/v1/labs/fluid-ui/requests` | Body `{"prompt": "..."}` (at most 2000 bytes). Starts a workflow run and returns the request (202). |
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

1. `load-request` (`goobers fluid-ui-request --load`) loads the request this
   run serves, your current customization, and a description of the
   customization surface and its limits.
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

The request text is stored only in your private request file and is passed to
the run that serves it. It is not part of the trigger key or the run's
outputs, and it never enters any aggregate; only normalized unmet-need labels
are meant to be counted across users. The request text does appear in that
run's `load-request` artifact and in the agent transcript, which anyone who
can read the run can see.
