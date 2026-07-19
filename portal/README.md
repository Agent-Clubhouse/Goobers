# Goobers portal

This directory contains the production UI for the Dashboard / Portal milestone
(#14, epic #440).

It is a React/Vite application with reusable shell, navigation, dense-list,
graph, inspector, status, icon, theme, accessibility, query-state, and typed
daemon-client modules. Overview, Workflows, and run detail read the live daemon
API, as do configuration warnings on the overview and workflow routes; the
remaining prototype routes use static fixtures until their vertical slices land.

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

The production build and component tests are:

```bash
npm --prefix portal test
npm --prefix portal run build
```

The production build writes `cmd/goobers/portal-dist`, which is embedded in the
`goobers` binary.

## Feedback paths

- **Overview**: attention-first operations view, active runs, recent outcomes,
  instance warning, and daemon freshness.
- **Workflows**: dense inventory, gaggle/goober context, and workflow detail
  with selectable stages.
- **Runs**: status filters and run detail.
- **Run detail**: pinned identity, synchronized execution graph, and durable
  event ledger.
- **Theme**: independently tuned light and dark palettes.

Daemon fixtures cover live and terminal runs, repasses, and forward-compatible
unknown journal events.

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

- Workflow detail still uses static data intentionally shaped around a
  representative workflow. Overview, workflow inventory, gaggle rosters, run
  detail, and configuration warnings use the HTTP daemon adapter, with the
  fixture adapter reserved for tests.
- Attempt, artifact, replay, and escalation views remain deferred to their
  dedicated portal slices.
- Run detail uses the pinned graph topology and an ordered stage presentation
  that remains operable at narrow widths.
- Tier-1 is localhost-only and does not activate the future MSAL/OIDC scaffold.

Production issues must preserve accepted behavior while replacing fixtures with
the shared versioned daemon API.
