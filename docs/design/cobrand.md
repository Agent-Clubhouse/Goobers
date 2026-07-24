# Design: Dashboard co-branding and support hooks

> Status: **Approved for implementation**
> Area prefix: `CBR`
> Milestone: **V1 — arbitrary repos / teams / hardening**
> Related: [`docs/design/dashboard.md`](dashboard.md) · [`docs/requirements/portal.md`](../requirements/portal.md) · [`docs/requirements/instance.md`](../requirements/instance.md)

## 1. Problem

Goobers is self-hosted. Every deployment team runs the product under their own
organization and wants the dashboard to reflect that — their name, their logo,
their color accent, and most importantly their own support channels. Today the
portal hardcodes the Goobers brand (mascot, "goobers", "local operations") and
provides no surface for directing users to team-specific docs or help.

## 2. Scope

**In scope:**

- A `portal:` block in `instance.yaml` that declares brand identity, optional
  color overrides, and support links.
- A new read-only `GET /api/v1/portal/config` API endpoint that surfaces the
  effective cobrand config to the portal.
- Portal reads the config on load and applies it: brand text, logo, accent
  color tokens, and a support footer in the sidebar.
- Static asset serving for logo and favicon from a per-instance `assets/`
  directory.
- Full-graceful defaults: an unconfigured instance shows standard Goobers
  branding, zero regression.

**Out of scope:**

- Per-gaggle theme overrides (branding is instance-wide identity, not per-team
  style).
- Full white-label (removing the upstream attribution footer).
- Interactive theme editor in the portal (config-as-code only).
- CSS `@font-face` injection or custom typefaces.
- Dark/light palette replacement beyond the semantic accent token pair.

## 3. Config schema

Add a `portal:` section to `instance.yaml`. All fields are optional; absent
fields fall back to the built-in Goobers defaults shown in the comments.

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
# ... existing fields ...

portal:
  brand:
    # Human-readable name for this deployment. Shown in the sidebar header and
    # browser tab title. Default: "goobers"
    name: "Acme Ops"

    # One-line tagline shown under the name in the sidebar header.
    # Default: "local operations"
    tagline: "AI workforce platform"

    # Single character (or short grapheme) used as the scope mark in the
    # topbar. Default: first character of brand.name, upper-cased.
    scopeMark: "A"

    # Path to a logo image served by the daemon from the instance assets/
    # directory (see §6). Replaces the Goobers mascot in the sidebar header.
    # Omit to keep the mascot. Must start with /assets/.
    # Example: /assets/logo.svg
    logoUrl: ""

    # Path to a favicon served from assets/. Omit to keep the default.
    # Must start with /assets/. Example: /assets/favicon.ico
    faviconUrl: ""

  theme:
    # Accent color token overrides. Each value must be a valid CSS color (hex,
    # rgb(), hsl(), or named). Only the accent family is overridable; semantic
    # status tokens (success, warning, danger) are intentionally fixed.
    #
    # Light-mode accent (replaces --accent: #6847d9). Used for active nav
    # items, links, buttons, focus rings, and highlights.
    accentLight: ""

    # Dark-mode accent (replaces --accent: #a98cff).
    accentDark: ""

    # Soft accent for light mode (replaces --accent-soft: #eee9ff).
    # Background of accent-tinted chips and badges.
    accentSoftLight: ""

    # Soft accent for dark mode (replaces --accent-soft: #2c2445).
    accentSoftDark: ""

    # Ink-on-accent for light mode (replaces --accent-ink: #4c2db8).
    # Text color used on accent-background surfaces.
    accentInkLight: ""

    # Ink-on-accent for dark mode (replaces --accent-ink: #c5b4ff).
    accentInkDark: ""

  support:
    # URL for primary documentation. Shown as a "Docs" link in the sidebar
    # support footer. Omit to hide.
    docsUrl: "https://acme.example/docs/goobers"

    # URL for filing issues or contacting support. Shown as a "Get help" link.
    # Omit to hide.
    issuesUrl: "https://acme.example/support"

    # URL for a Slack channel, Teams deep-link, or other chat surface.
    # Shown as a "Chat" link. Omit to hide.
    chatUrl: "slack://channel/C000EXAMPLE"

    # Additional arbitrary links shown in the support footer, in order.
    # Maximum 6 entries.
    links:
      - label: "Runbooks"
        url: "https://acme.example/runbooks"
      - label: "On-call"
        url: "https://acme.example/oncall"
```

### 3.1 Validation rules

The following are enforced by `goobers validate` and at daemon startup:

| Field | Rule |
|---|---|
| `brand.name` | Non-empty string ≤ 64 chars when present. |
| `brand.tagline` | Non-empty string ≤ 128 chars when present. |
| `brand.scopeMark` | Single Unicode grapheme cluster when present. |
| `brand.logoUrl` | Must begin with `/assets/` when present. |
| `brand.faviconUrl` | Must begin with `/assets/` when present. |
| `theme.accent*` | Valid CSS color string when present (hex `#rrggbb`/`#rgb`, `rgb()`, `hsl()`, or a CSS named color). |
| `support.docsUrl` | Absolute HTTPS URL when present. |
| `support.issuesUrl` | Absolute HTTPS URL when present. |
| `support.chatUrl` | Absolute URL (`https://` or known deep-link schemes: `slack://`, `msteams://`) when present. |
| `support.links[].label` | Non-empty string ≤ 32 chars. |
| `support.links[].url` | Absolute HTTPS URL. |
| `support.links` | Maximum 6 entries. |

`logoUrl` and `faviconUrl` paths are validated to start with `/assets/` to
prevent the portal from fetching arbitrary remote URLs or daemon-internal paths
via the brand config. The daemon serves `/assets/` from `<instance-root>/assets/`
(see §6).

## 4. API contract

### 4.1 New endpoint

```
GET /api/v1/portal/config
```

Returns the effective cobrand configuration for the portal. This endpoint is
always available (even standalone mode) and requires no authentication beyond
the loopback binding. The response is cache-friendly: the `ETag` reflects the
raw config section's digest and the daemon sets `Cache-Control: no-cache`
(revalidation on every page load, not full re-fetch).

**Response shape** (new `PortalConfig` type added to the API contract):

```typescript
interface PortalConfig {
  brand: {
    name: string;          // effective value (defaults applied)
    tagline: string;       // effective value
    scopeMark: string;     // effective value
    logoUrl: string | null;
    faviconUrl: string | null;
  };
  theme: {
    accentLight: string | null;
    accentDark: string | null;
    accentSoftLight: string | null;
    accentSoftDark: string | null;
    accentInkLight: string | null;
    accentInkDark: string | null;
  };
  support: {
    docsUrl: string | null;
    issuesUrl: string | null;
    chatUrl: string | null;
    links: Array<{ label: string; url: string }>;
  };
}
```

Defaults (when fields are absent from `instance.yaml`):

| Field | Default |
|---|---|
| `brand.name` | `"goobers"` |
| `brand.tagline` | `"local operations"` |
| `brand.scopeMark` | `"G"` |
| `brand.logoUrl` | `null` (portal renders mascot) |
| `brand.faviconUrl` | `null` (portal uses built-in favicon) |
| `theme.*` | `null` (portal uses built-in CSS tokens) |
| `support.*` | `null` / `[]` (no support section rendered) |

### 4.2 Go wire fixture update

The `GoWireFixtures` type and the `goWireFixtures` fixture object in
`portal/src/api/wire.generated.ts` gain a `portalConfig` entry. The generator
in `internal/apicontract` is extended accordingly. The fixture uses default
values (no cobrand), preserving existing snapshot behavior for all other tests.

### 4.3 `DaemonClient` interface extension

```typescript
interface DaemonClient {
  // ... existing methods ...
  getPortalConfig(options?: RequestOptions): Promise<PortalConfig>;
}
```

## 5. Portal implementation

### 5.1 `CobrandContext`

A new `CobrandContext` (`portal/src/cobrand.tsx`) wraps the application and
exposes the fetched `PortalConfig`. The fetch runs once at app init
(`App.tsx`) alongside the initial instance fetch. A loading state shows the
current built-in defaults (no flash of wrong brand).

```tsx
// portal/src/cobrand.tsx
export interface CobrandContextValue {
  config: PortalConfig;
  loading: boolean;
}

export const CobrandContext = createContext<CobrandContextValue>(...defaultValue);
export function useCobrand(): CobrandContextValue { ... }
```

The context value is stable after the first successful fetch. If the fetch
fails, the context silently falls back to full Goobers defaults (no error
surface in the portal; a validation warning appears in the instance status
panel if the config is structurally invalid).

### 5.2 CSS token injection

When `theme.accentLight` or `theme.accentDark` is non-null, the portal injects
a `<style>` element with overriding CSS custom properties into `<head>` before
the first render. The injection is keyed to the current theme mode so light-mode
overrides only apply under `:root:not([data-theme="dark"])` and dark-mode
overrides only apply under `:root[data-theme="dark"]`.

```ts
// portal/src/cobrand.tsx — applyThemeOverrides(config, theme)
function buildOverrideStyle(config: PortalConfig, theme: Theme): string {
  const light: string[] = [];
  const dark: string[] = [];
  if (config.theme.accentLight)     light.push(`--accent: ${config.theme.accentLight}`);
  if (config.theme.accentSoftLight) light.push(`--accent-soft: ${config.theme.accentSoftLight}`);
  if (config.theme.accentInkLight)  light.push(`--accent-ink: ${config.theme.accentInkLight}`);
  if (config.theme.accentDark)      dark.push(`--accent: ${config.theme.accentDark}`);
  if (config.theme.accentSoftDark)  dark.push(`--accent-soft: ${config.theme.accentSoftDark}`);
  if (config.theme.accentInkDark)   dark.push(`--accent-ink: ${config.theme.accentInkDark}`);
  // ...
}
```

The injected `<style>` element has `id="cobrand-theme"` so it can be replaced
in place when the portal-config fetch resolves (avoids a flash).

### 5.3 `PortalShell` changes

`PortalShell.tsx` calls `useCobrand()` and:

- Replaces the hardcoded `"goobers"` and `"local operations"` strings with
  `config.brand.name` and `config.brand.tagline`.
- Replaces the hardcoded `"G"` scope mark with `config.brand.scopeMark`.
- Renders `<img src={config.brand.logoUrl} />` instead of the mascot when
  `logoUrl` is non-null; otherwise keeps the mascot image.
- Sets `document.title` to `config.brand.name` on mount.
- Renders a `<SupportFooter />` component at the bottom of the sidebar when
  any support URL or link is present.

### 5.4 `SupportFooter` component

```tsx
// portal/src/shell/SupportFooter.tsx
// Renders in the sidebar below the status area when any support field is set.
// Shows up to: Docs, Get help, Chat, and custom links.
// Each opens in a new tab (rel="noopener noreferrer").
// Hidden entirely when no support fields are configured.
```

The footer is visually de-emphasized (muted ink, smaller type) to avoid
competing with the operational nav. It is accessible: each link has a
descriptive `aria-label` and the section is labeled as a `<nav
aria-label="Support">`.

### 5.5 Favicon

When `brand.faviconUrl` is non-null, the portal updates the `<link
rel="icon">` element in `<head>` to point at the custom URL. This runs after
the cobrand fetch resolves and is a no-op when null.

## 6. Static asset serving

The daemon's HTTP layer gains a new route:

```
GET /assets/<path>
```

This serves files from `<instance-root>/assets/` with:

- **Path traversal prevention:** the resolved path must remain under the
  `assets/` subdirectory of the instance root (no `../` escapes).
- **MIME sniffing:** standard `http.ServeContent` MIME detection.
- **Content-Security-Policy:** the existing CSP for the portal does not need
  to change since `/assets/` is same-origin.
- **No directory listing:** 404 for directory requests.

`goobers init` does not create `assets/` (optional directory). Operators
create it and place images there. `goobers validate` warns (not errors) if
`brand.logoUrl` or `brand.faviconUrl` reference an asset that does not exist
on disk at validation time.

## 7. `goobers validate` additions

Two new validation warnings (not errors, since the daemon still starts):

| Code | Condition |
|---|---|
| `CBR001` | `brand.logoUrl` is set but the file does not exist under `assets/`. |
| `CBR002` | `brand.faviconUrl` is set but the file does not exist under `assets/`. |

A new validation error (daemon refuses to start):

| Code | Condition |
|---|---|
| `CBR003` | Any `theme.accent*` value is present but fails CSS color validation. |

## 8. Config-examples update

The `config-examples/manifest.yaml` gains a commented-out `portal:` block
illustrating the full schema so that teams scaffolding a new instance have a
reference in the file they're already editing.

## 9. Upgrade path / compatibility

- `portal:` is an entirely new, optional section. All existing `instance.yaml`
  files remain valid and gain Goobers-default behavior.
- The `GET /api/v1/portal/config` endpoint is additive. Old portals (pre-CBR)
  simply never call it.
- Wire fixture versioning: the new fixture key `portalConfig` is additive; the
  generator is append-only for this release.

## 10. Security considerations

- Asset URLs in the cobrand config are validated to begin with `/assets/`
  (relative, same-origin) to prevent the portal from loading arbitrary remote
  resources via the brand config. Support URLs are validated to be absolute
  HTTPS (or known deep-link schemes) so they cannot be `javascript:` URIs or
  `file://` paths.
- The `/assets/` handler enforces path-traversal prevention; it never resolves
  symlinks outside the instance root.
- The cobrand config is read from `instance.yaml`, which is operator-controlled
  and local to the machine running the daemon. No user-supplied input reaches it.

## 11. Implementation checklist

- [ ] `internal/instance/config.go` — add `PortalConfig`, `PortalBrandConfig`,
      `PortalThemeConfig`, `PortalSupportConfig`, `PortalSupportLink` structs;
      add `Portal PortalConfig` field to `Config`; add validation logic for
      `CBR001`–`CBR003`.
- [ ] `internal/apicontract/` — add `PortalConfig` to the Go contract definition;
      run `make generate` to update `portal/src/api/wire.generated.ts` and
      `portal/src/api/contract.generated.ts`.
- [ ] HTTP handler — add `GET /api/v1/portal/config` handler; add
      `/assets/<path>` static-serve handler.
- [ ] `portal/src/api/types.ts` — add `PortalConfig` TypeScript type.
- [ ] `portal/src/api/httpClient.ts` — implement `getPortalConfig()`.
- [ ] `portal/src/cobrand.tsx` — `CobrandContext`, `useCobrand()`,
      `applyThemeOverrides()`.
- [ ] `portal/src/App.tsx` — fetch portal config on init; wrap with
      `CobrandContext.Provider`.
- [ ] `portal/src/shell/PortalShell.tsx` — consume `useCobrand()`;
      replace hardcoded brand strings; add `<SupportFooter />`.
- [ ] `portal/src/shell/SupportFooter.tsx` — new component.
- [ ] `portal/src/tokens.css` — no change required (overrides injected via
      `<style>` element, not by editing the stylesheet).
- [ ] `portal/src/theme.ts` — expose a `useThemeWithCobrand()` variant or
      extend `useTheme()` to re-apply CSS overrides on theme toggle.
- [ ] `config-examples/manifest.yaml` — add commented cobrand block.
- [ ] `docs/guides/` — add `cobrand.md` operator guide.
- [ ] README updates (see §12).

## 12. README surface

- **`portal/README.md`** — add "Co-branding and support hooks" section
  describing the `portal:` config block, asset serving, and support footer.
- **`README.md`** — update portal row in the repository layout table to note
  cobrand support.
- **`docs/requirements/portal.md`** — add `PORT-CBR` requirements block.
