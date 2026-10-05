# Goobers portal

This directory contains the production UI for the Dashboard / Portal milestone
(#14, epic #440).

It is a React/Vite application with reusable shell, navigation, dense-list,
graph, inspector, status, icon, theme, accessibility, query-state, and typed
daemon-client modules. Overview, workflow inventory and detail, and run detail
read the live daemon API, as do configuration warnings on the overview and
workflow routes; the remaining prototype routes use static fixtures until their
vertical slices land.

## Run it

From an instance root, the production portal is one command:

```bash
goobers dashboard
```

Use `--no-open` to print the URL without launching a browser, `--port=auto` to
increment past a conflict, and `--dev-assets=<dir>` to serve an alternate portal
build. The command attaches to a live `goobers up` API when available and
otherwise starts a standalone read-only service.

For Vite development:

```bash
npm --prefix portal install
npm --prefix portal run dev
```

The Vite development server proxies same-origin `/api` requests to the
`goobers up` daemon at `http://127.0.0.1:8080` by default. If `api.listen`
uses another address, set the proxy target when starting the portal:

```bash
GOOBERS_DAEMON_URL=http://127.0.0.1:9090 npm --prefix portal run dev
```

### Getting Started UI development

Do not rebuild and restart the embedded portal after every React, CSS, or image
change. Build the artifact once so the tagged Go command can compile, then keep
the Go backend running in one PowerShell terminal:

```powershell
npm --prefix portal run build
go run .\cmd\goobers init --guided --dev-assets=internal\portalassets\dist --port=8081 --no-open
```

Start the dedicated Vite mode in another terminal:

```powershell
npm --prefix portal run dev:getting-started
```

Open `http://127.0.0.1:5173/`. Vite stamps the page as
`getting-started`, proxies `/guided/*` to `http://127.0.0.1:8081`, and
hot-reloads frontend changes. Set `GOOBERS_GUIDED_URL` before starting Vite if
the backend uses another address.

Restart the Go process only after changing Go handlers or other backend code.
When the UI is ready for final validation, run `npm --prefix portal run build`
once and restart `goobers init --guided` so the process serves the newly
embedded asset artifact.

The portal CI gate is reproduced locally with:

```bash
make portal-ci
```

This lockfile-installs dependencies, type-checks, builds, runs the portal tests,
and verifies the generated Go wire fixtures. To reproduce only the Go/TypeScript
contract gate, including fixture regeneration and the stale-output diff:

```bash
make portal-contract
```

`make generate` intentionally updates the checked-in route contract and wire
fixtures after a Go contract change.

The production build writes the ignored
`internal/portalassets/dist` directory. This directory is the reusable static
asset artifact: complete Goobers builds embed it with the `embed_portal` build
tag, while another host can copy or serve the same files directly. Its
`portal-artifact.json` records the Portal version, producing commit, API
contract version, and required route roots.

The hosting contract is intentionally small:

- serve the artifact at the origin root;
- route `/api` and `/guided` to compatible Goobers backends on the same origin;
- preserve the `goobers-dashboard-mode` meta element in `index.html` if the
  host needs to select another supported mode.

Use `make build-goobers` for a complete binary. Plain Go builds and tests do
not require Node, but their binary does not contain the Portal and reports that
clearly if the dashboard is started.

## Feedback paths

- **Overview**: attention-first operations view, active runs, recent outcomes,
  instance warning, and daemon freshness.
- **Needs attention**: blocked and stalled work precedes muted FYI failures.
  Severity uses current work-item labels and runner liveness, not just phase.
  The list displays up to 20 runs from bounded recent candidates (including
  up to 100 failures); when candidate pages are truncated, a warning directs
  operators to the Runs page because additional actionable runs may exist.
- **Workflows**: dense inventory, gaggle/goober context, and workflow detail
  with selectable stages.
- **Runs**: status filters and run detail.
- **Run detail**: pinned identity, synchronized execution graph, and durable
  event ledger.
- **Theme**: independently tuned light and dark palettes.
- **Cost chart**: total AI credits per rolling 24-hour day as bars on the left
  axis, with P50 and P95 per-stage-attempt cost as lines on an independently scaled right axis.
  The 24h, 7d, and 30d windows contain 1, 7, and 30 daily buckets respectively.
  Custom hover and keyboard-focus tooltips show the day, series, and cost.
- **Scope selection**: Cost and Insight share a searchable nested picker.
  Select a gaggle or workflow by clicking its name; use its arrow to expand
  workflows or stages. Run totals and measured stage attempts are reported separately.
- **Overview**: a fixed page title with an animated loading suffix and resolved
  operational status. Data freshness and refresh sit beside the active-run count.
- **Co-branding**: operator-configurable name, logo, accent colors, and support
  links via `instance.yaml`.

Daemon fixtures cover live and terminal runs, repasses, and forward-compatible
unknown journal events.

## Co-branding and support hooks

### Shared UI conventions

Use the exported primitives in `src/ui/` rather than copying page-specific
markup: `PageHeading`/`SectionHeading`, `Action`/`ActionLink`, `ControlGroup`,
`FilterField`/`FilterOptions`/`FilterOption`, `TabList`/`Tab`, `MetadataGrid`,
and `DataTable`. Interactive linked lists use `DataList`/`DataRow`; both native
tables and linked lists share the same table shell, header, and typography.
Keep layout-specific CSS on the page, but keep font sizes, colors, and control
treatment in the primitives and `tokens.css`.

The first content block below a page heading or toolbar uses
`--space-page-content` (1rem by default). Override this shared token for themed
spacing rather than adding page-specific margins. Later sections retain their
own grouping space. Fields inside `ControlGroup` have no outer padding or margin,
so their labels and controls align without per-page offsets.

At 820px and below (and on short landscape screens), navigation uses a 56px
top bar with the current area and a menu containing every destination. The
desktop sidebar remains unchanged; there is no intermediate navigation grid or
bottom navigation bar. The menu keeps native dialog focus and history behavior,
and its scrolling content is inside the rounded sheet so scrollbars cannot
square off its corners.

The type scale is page title, section title, 14px UI text, 12px dense table and
secondary text, and 11px uppercase labels, expressed as rem-based tokens.
Primary actions use `--accent` and `--on-accent`; links use `--accent-ink`.
Semantic success/warning/error colors remain separate from co-brand accents.
The primitive stylesheet is included by both Vite and the reusable package.

Use `Timestamp` for visible dates and `dateTime.ts` for dates embedded in
messages. The shared convention is English month/day/year with local time and
seconds; precise tooltips include the timezone. Chart ticks use the same
formatter's compact date/hour variants. Missing and invalid dates are explicit,
and valid dates retain a machine-readable `datetime`. Do not add local
`Intl.DateTimeFormat` or `toLocaleDateString` implementations to pages.

The portal reads a `GET /api/v1/portal/config` endpoint at startup and applies
operator-supplied identity and support links from the instance's `portal:` config
block. All fields are optional; an unconfigured instance shows standard Goobers
branding with zero changes needed.

**Brand identity** — set in `instance.yaml` under `portal.brand`:

```yaml
portal:
  brand:
    name: "Acme Ops"
    tagline: "AI workforce platform"
    scopeMark: "A"
    logoUrl: "/assets/logo.svg"     # served from <instance-root>/assets/
    faviconUrl: "/assets/favicon.ico"
```

**Accent color overrides** — replace the default purple accent in light and/or
dark mode. Only the accent token family is overridable; semantic status tokens
(success, warning, danger) are fixed.

```yaml
portal:
  theme:
    accentLight: "#0078d4"
    accentDark: "#4fa3e3"
    accentSoftLight: "#cce4f6"
    accentSoftDark: "#1a3a52"
    accentInkLight: "#004578"
    accentInkDark: "#a8d4f5"
```

**Support links** — shown in a de-emphasized footer at the bottom of the sidebar
when any field is set. Provides Docs, Get help, Chat, and up to 6 custom links.

```yaml
portal:
  support:
    docsUrl: "https://acme.example/docs/goobers"
    issuesUrl: "https://acme.example/support"
    chatUrl: "slack://channel/C000EXAMPLE"
    links:
      - label: "Runbooks"
        url: "https://acme.example/runbooks"
```

Logo and favicon images are served from the `assets/` subdirectory of the
instance root. Create the directory and place images there; no daemon restart is
required for asset file changes (assets are served on-demand). `goobers validate`
warns if a referenced asset file does not exist.

Full design details: [`docs/design/cobrand.md`](../docs/design/cobrand.md).

## Accepted design decisions

- Workbench, not command center.
- Ledger, not chat.
- Mascot as a restrained identity anchor, not an agent avatar.
- Purple as an accent, not a generic AI gradient.
- Dense operational lists over placeholder metric cards.
- Graph for structure; ordered journal ledger for time and causality.
- Attempts and artifacts as first-class review objects.
- No dead future controls.
- Motion only when it explains state, with reduced-motion support.

The full product and architecture authority is
[`docs/design/dashboard.md`](../docs/design/dashboard.md).

## Current boundaries

- Workflow detail uses the current definition, canonical graph, stage summaries,
  and recent runs from the HTTP daemon adapter. The fixture adapter is reserved
  for tests.
- Attempt, artifact, replay, and escalation views remain deferred to their
  dedicated portal slices.
- Run detail uses the pinned graph topology and an ordered stage presentation
  that remains operable at narrow widths.
- Tier-1 is localhost-only and does not activate the future MSAL/OIDC scaffold.

Production issues must preserve accepted behavior while replacing fixtures with
the shared versioned daemon API.
