import { useEffect, useState } from "react";

export type Theme = "light" | "dark";

const themeStorageKey = "goobers-theme";

function localStorageOrUndefined(): Storage | undefined {
  try {
    const storage = window.localStorage;
    if (typeof storage?.getItem !== "function" || typeof storage.setItem !== "function") {
      return undefined;
    }
    return storage;
  } catch (error) {
    if (error instanceof DOMException) {
      return undefined;
    }
    throw error;
  }
}

export function storedTheme(storage = localStorageOrUndefined()): Theme {
  return storage?.getItem(themeStorageKey) === "dark" ? "dark" : "light";
}

export function persistTheme(theme: Theme, storage = localStorageOrUndefined()) {
  try {
    storage?.setItem(themeStorageKey, theme);
  } catch (error) {
    if (!(error instanceof DOMException)) {
      throw error;
    }
  }
}

export function useTheme() {
  const [theme, setTheme] = useState<Theme>(() => storedTheme());

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    persistTheme(theme);
  }, [theme]);

  return [theme, setTheme] as const;
}
