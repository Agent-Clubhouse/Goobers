# Goobers portal

This directory contains the production UI foundations for the Dashboard /
Portal milestone (#14, epic #440).

The Overview, Workflows, and Runs routes are backed by static fixtures until
the versioned daemon client lands. The shell, visual primitives, theme tokens,
responsive behavior, and accessibility support are production code.

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
- Artifact rows demonstrate hierarchy but do not open real journal content.
- Tier-1 is localhost-only; authentication is not part of this foundation.

Later portal slices replace fixtures through the shared versioned daemon API
without changing these presentation foundations.
