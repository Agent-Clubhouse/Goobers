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
- **Co-branding**: operator-configurable name, logo, accent colors, and support
  links via `instance.yaml`.

Daemon fixtures cover live and terminal runs, repasses, and forward-compatible
unknown journal events.

## Co-branding and support hooks

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
