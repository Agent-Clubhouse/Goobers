import { createContext, useContext, useLayoutEffect, useMemo, useState } from "react";

export type Theme = "light" | "dark";

const themeStorageKey = "goobers-theme";

interface ThemeContextValue {
  theme: Theme;
  toggleTheme: () => void;
}

const ThemeContext = createContext<ThemeContextValue | undefined>(undefined);

function browserStorage(): Storage | undefined {
  try {
    return window.localStorage;
  } catch (error) {
    console.warn("Theme preference storage is unavailable.", error);
    return undefined;
  }
}

export function storedTheme(): Theme {
  try {
    return browserStorage()?.getItem(themeStorageKey) === "dark" ? "dark" : "light";
  } catch (error) {
    console.warn("Theme preference could not be read.", error);
    return "light";
  }
}

function persistTheme(theme: Theme) {
  try {
    browserStorage()?.setItem(themeStorageKey, theme);
  } catch (error) {
    console.warn("Theme preference could not be saved.", error);
  }
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [theme, setTheme] = useState<Theme>(storedTheme);

  useLayoutEffect(() => {
    document.documentElement.dataset.theme = theme;
    persistTheme(theme);
  }, [theme]);

  const value = useMemo(
    () => ({
      theme,
      toggleTheme: () => setTheme((current) => (current === "light" ? "dark" : "light")),
    }),
    [theme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const context = useContext(ThemeContext);
  if (!context) {
    throw new Error("useTheme must be used within ThemeProvider");
  }
  return context;
}
