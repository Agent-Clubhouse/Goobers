import { createContext, useContext } from "react";
import type { PortalConfig } from "./api/types";
import type { Theme } from "./theme";

export const defaultPortalConfig: PortalConfig = {
  brand: {
    name: "goobers",
    tagline: "local operations",
    scopeMark: "G",
    logoUrl: null,
    faviconUrl: null,
  },
  theme: {
    accentLight: null,
    accentDark: null,
    accentSoftLight: null,
    accentSoftDark: null,
    accentInkLight: null,
    accentInkDark: null,
  },
  support: {
    docsUrl: null,
    issuesUrl: null,
    chatUrl: null,
    links: [],
  },
};

export interface CobrandContextValue {
  config: PortalConfig;
  loading: boolean;
}

export const CobrandContext = createContext<CobrandContextValue>({
  config: defaultPortalConfig,
  loading: true,
});

export function useCobrand(): CobrandContextValue {
  return useContext(CobrandContext);
}

export function applyThemeOverrides(config: PortalConfig, theme: Theme): void {
  const styleId = "cobrand-theme";
  let el = document.getElementById(styleId) as HTMLStyleElement | null;
  const light: string[] = [];
  const dark: string[] = [];
  if (config.theme.accentLight) light.push(`--accent: ${config.theme.accentLight}`);
  if (config.theme.accentSoftLight) light.push(`--accent-soft: ${config.theme.accentSoftLight}`);
  if (config.theme.accentInkLight) light.push(`--accent-ink: ${config.theme.accentInkLight}`);
  if (config.theme.accentDark) dark.push(`--accent: ${config.theme.accentDark}`);
  if (config.theme.accentSoftDark) dark.push(`--accent-soft: ${config.theme.accentSoftDark}`);
  if (config.theme.accentInkDark) dark.push(`--accent-ink: ${config.theme.accentInkDark}`);

  if (light.length === 0 && dark.length === 0) {
    el?.remove();
    return;
  }
  const css = [
    light.length ? `:root:not([data-theme="dark"]) { ${light.join("; ")} }` : "",
    dark.length ? `:root[data-theme="dark"] { ${dark.join("; ")} }` : "",
  ]
    .filter(Boolean)
    .join("\n");

  if (!el) {
    el = document.createElement("style");
    el.id = styleId;
    document.head.appendChild(el);
  }
  el.textContent = css;
  void theme;
}
