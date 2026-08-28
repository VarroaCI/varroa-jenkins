import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { useTheme } from "./ThemeContext";

// Mock bffFetch so AuthProvider inside renderWithProviders doesn't error.
const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: vi.fn(() => null),
  logout: vi.fn(),
}));

beforeEach(() => {
  localStorage.clear();
  mockBffFetch.mockResolvedValue({});
  // Reset dataset attributes set by previous tests.
  delete document.documentElement.dataset.theme;
  delete document.documentElement.dataset.accent;
});

// ---- Test consumer component ----

function ThemeTestComponent() {
  const { theme, accent, setTheme, setAccent } = useTheme();
  return (
    <div>
      <span data-testid="theme">{theme}</span>
      <span data-testid="accent">{accent}</span>
      <button data-testid="set-dark" onClick={() => setTheme("dark")}>
        Dark
      </button>
      <button data-testid="set-rust" onClick={() => setAccent("rust")}>
        Rust
      </button>
    </div>
  );
}

// ---- Tests ----

describe("ThemeContext", () => {
  it("provides default theme 'light' and default accent 'honey'", () => {
    renderWithProviders(<ThemeTestComponent />);

    expect(screen.getByTestId("theme")).toHaveTextContent("light");
    expect(screen.getByTestId("accent")).toHaveTextContent("honey");
  });

  it("setTheme updates the theme", () => {
    renderWithProviders(<ThemeTestComponent />);

    act(() => {
      screen.getByTestId("set-dark").click();
    });

    expect(screen.getByTestId("theme")).toHaveTextContent("dark");
  });

  it("setAccent updates the accent", () => {
    renderWithProviders(<ThemeTestComponent />);

    act(() => {
      screen.getByTestId("set-rust").click();
    });

    expect(screen.getByTestId("accent")).toHaveTextContent("rust");
  });

  it("syncs theme to document.documentElement.dataset.theme", () => {
    renderWithProviders(<ThemeTestComponent />);

    act(() => {
      screen.getByTestId("set-dark").click();
    });

    expect(document.documentElement.dataset.theme).toBe("dark");
  });

  it("syncs accent to document.documentElement.dataset.accent", () => {
    renderWithProviders(<ThemeTestComponent />);

    act(() => {
      screen.getByTestId("set-rust").click();
    });

    expect(document.documentElement.dataset.accent).toBe("rust");
  });

  it("persists theme to localStorage", () => {
    renderWithProviders(<ThemeTestComponent />);

    act(() => {
      screen.getByTestId("set-dark").click();
    });

    expect(localStorage.getItem("varroa-theme")).toBe("dark");
  });

  it("persists accent to localStorage", () => {
    renderWithProviders(<ThemeTestComponent />);

    act(() => {
      screen.getByTestId("set-rust").click();
    });

    expect(localStorage.getItem("varroa-accent")).toBe("rust");
  });

  it("throws when used outside ThemeProvider", () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    function BadComponent() {
      let msg = "";
      try {
        useTheme();
      } catch (e) {
        msg = (e as Error).message;
      }
      return <div data-testid="err">{msg}</div>;
    }

    render(<BadComponent />);

    expect(screen.getByTestId("err")).toHaveTextContent(
      "useTheme must be used within ThemeProvider",
    );
    errSpy.mockRestore();
  });
});
