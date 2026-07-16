import { useEffect, useState } from "react";

export type Theme = "light" | "dark";
export const themeStorageKey = "goobers-theme";

export function storedTheme(): Theme {
  try {
    return window.localStorage?.getItem(themeStorageKey) === "dark" ? "dark" : "light";
  } catch {
    return "light";
  }
}

function persistTheme(theme: Theme) {
  try {
    window.localStorage?.setItem(themeStorageKey, theme);
  } catch {
    // Storage can be unavailable in private or constrained browser contexts.
  }
}

export function useTheme() {
  const [theme, setTheme] = useState<Theme>(() => storedTheme());

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    persistTheme(theme);
  }, [theme]);

  return {
    theme,
    toggleTheme: () => setTheme((current) => (current === "light" ? "dark" : "light")),
  };
}
