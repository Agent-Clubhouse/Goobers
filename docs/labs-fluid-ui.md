# Labs: Fluid UI customizations

Fluid UI (#8338) lets each Portal user keep their own presentation
customization. This page documents the store and API (#8415) and how the
Portal applies a customization (#8416).

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
