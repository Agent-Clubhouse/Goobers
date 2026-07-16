# Goobers dashboard prototype

This directory contains the accepted V1 interaction and visual reference for
the Dashboard / Portal milestone (#14, epic #440).

It is a real React/Vite click-through backed by static fixtures. It is design
collateral, not the production daemon client.

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

## Prototype boundaries

- Data is static and intentionally shaped around representative runs.
- The graph layout is fixture-specific, not a general layout engine.
- Components are intentionally colocated for iteration speed and may be split
  during productionization.
- Artifact previews use static fixture content; production will load the same safe
  text/JSON views and download-only media through the shared daemon API.
- The responsive treatment is a baseline; production work will generalize graph
  layout beyond the fixed prototype fixtures.
- Tier-1 is localhost-only and does not activate the future MSAL/OIDC scaffold.

Production issues must preserve accepted behavior while replacing fixtures with
the shared versioned daemon API.
