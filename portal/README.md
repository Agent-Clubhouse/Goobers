# Goobers portal

This directory contains the production UI foundation and static fixture views
for the Dashboard / Portal milestone (#14, epic #440).

It is a React/Vite application with reusable shell, navigation, dense-list,
graph, inspector, status, icon, theme, accessibility, query-state, and typed
daemon-client modules. The current routes remain backed by static fixtures; the
daemon client is intentionally not connected to production pages yet.

## Run it

```bash
npm --prefix portal install
npm --prefix portal run dev
```

The production build and component tests are:

```bash
npm --prefix portal test
npm --prefix portal run build
```

## Feedback paths

- **Overview**: attention-first operations view, active runs, recent outcomes,
  instance warning, and daemon freshness.
- **Workflows**: dense inventory, gaggle/goober context, and workflow detail
  with selectable stages.
- **Runs**: status filters and run detail.
- **Run detail**: synchronized execution graph, event ledger, replay, attempts,
  outputs, artifacts, and escalation cause.
- **Theme**: independently tuned light and dark palettes.

The `Live visual dashboard and workflow DAG` fixture is the richest review
path. It contains three repasses and a terminal escalation.

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

- Production pages still use static data intentionally shaped around
  representative runs; the HTTP and fixture daemon adapters are data-foundation
  seams for later vertical slices.
- The graph layout is fixture-specific, not a general layout engine.
- Artifact rows demonstrate hierarchy but do not open real journal content.
- The graph layout remains fixture-specific; narrow layouts use an equivalent
  ordered stage presentation instead of clipping the desktop graph.
- Tier-1 is localhost-only and does not activate the future MSAL/OIDC scaffold.

Production issues must preserve accepted behavior while replacing fixtures with
the shared versioned daemon API.
