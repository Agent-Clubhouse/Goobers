# Goobers portal

This directory contains the production UI foundation for the Dashboard / Portal
milestone (#14, epic #440), preserving the accepted interaction and visual
reference.

It is a React/Vite application backed by static fixtures. The shell, navigation,
theme, icons, statuses, operational lists, graph frame, and inspectors are
reusable modules; daemon integration belongs to the typed-client slice.

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

- Data is static and intentionally shaped around representative runs.
- The graph layout is fixture-specific, not a general layout engine.
- Artifact rows demonstrate hierarchy but are not controls until safe journal
  content access is available.
- Responsive treatment keeps all controls usable at 320 px while preserving the
  desktop-first diagnostic workspace.
- Tier-1 is localhost-only and does not activate the future MSAL/OIDC scaffold.

Future vertical slices replace fixtures through the shared versioned daemon API;
the portal foundation does not read daemon or backend data directly.
