import { createContext, useContext, useState, useCallback, useEffect, type ReactNode } from "react";

type Theme = "light" | "dark";
type Accent = "honey" | "rust" | "pollen" | "propolis";

interface ThemeContextValue {
  theme: Theme;
  accent: Accent;
  setTheme: (t: Theme) => void;
  setAccent: (a: Accent) => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

const THEME_KEY = "varroa-theme";
const ACCENT_KEY = "varroa-accent";

function readStored(key: string, fallback: string): string {
  try {
    return localStorage.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(
    () => readStored(THEME_KEY, "light") as Theme,
  );
  const [accent, setAccentState] = useState<Accent>(
    () => readStored(ACCENT_KEY, "honey") as Accent,
  );

  // Sync state to DOM attributes and localStorage.
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    try { localStorage.setItem(THEME_KEY, theme); } catch { /* noop */ }
  }, [theme]);

  useEffect(() => {
    document.documentElement.dataset.accent = accent;
    try { localStorage.setItem(ACCENT_KEY, accent); } catch { /* noop */ }
  }, [accent]);

  const setTheme = useCallback((t: Theme) => setThemeState(t), []);
  const setAccent = useCallback((a: Accent) => setAccentState(a), []);

  return (
    <ThemeContext.Provider value={{ theme, accent, setTheme, setAccent }}>
      {children}
    </ThemeContext.Provider>
  );
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used within ThemeProvider");
  return ctx;
}
