import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../context/ThemeContext";
import { ToastProvider } from "./Toast";
import { createTestQueryClient } from "../test/render-utils";
import { Topbar } from "./Topbar";

// Topbar itself doesn't call useAuth, but ProfileMenu (rendered by Topbar) does.
// Provide a minimal AuthContext mock so ProfileMenu doesn't crash.
vi.mock("../context/AuthContext", () => {
  const AuthProvider = ({ children }: { children: React.ReactNode }) => <>{children}</>;
  return {
    useAuth: vi.fn(() => ({
      user: null,
      phase: "checkingSession" as const,
      isLoading: false,
      isAuthenticated: false,
      permissions: undefined,
      authMode: "local",
      login: vi.fn(),
      logout: vi.fn(),
      authError: undefined,
    })),
    AuthProvider,
  };
});

function renderTopbar(route = "/") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <ToastProvider>
            <Topbar />
          </ToastProvider>
        </ThemeProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("Topbar", () => {
  it("renders 'Dashboard' breadcrumb on root route", () => {
    renderTopbar("/");
    expect(screen.getByText("Dashboard")).toBeInTheDocument();
  });

  it("renders breadcrumb from path segment on /controllers", () => {
    renderTopbar("/controllers");
    expect(screen.getByText("Controllers")).toBeInTheDocument();
  });

  it("renders controller name in bold when on controller detail route", () => {
    renderTopbar("/controllers/core/default/my-controller");
    // For the controller detail match, breadcrumbs: "Controllers" (plain) / "core/default/my-controller" (bold)
    expect(screen.getByText("Controllers")).toBeInTheDocument();

    const boldEl = screen.getByText("core/default/my-controller");
    expect(boldEl).toBeInTheDocument();
    expect(boldEl.tagName).toBe("B");
  });

  it("renders CommandPalette toggle button and shortcut hint", () => {
    renderTopbar("/");
    // The search input placeholder hints at the shortcut
    const searchInput = screen.getByPlaceholderText(/⌘K/);
    expect(searchInput).toBeInTheDocument();
    // Also the explicit kbd element
    expect(screen.getByText("⌘K")).toBeInTheDocument();
  });

  it("renders the search input with placeholder", () => {
    renderTopbar("/");
    const input = screen.getByPlaceholderText(
      "Search controllers, namespaces, groups… (⌘K)",
    );
    expect(input).toBeInTheDocument();
  });

  it("renders activity feed button", () => {
    renderTopbar("/");
    const activityBtn = screen.getByTitle("Activity feed");
    expect(activityBtn).toBeInTheDocument();
  });

  it("renders theme toggle button", () => {
    renderTopbar("/");
    const themeBtn = screen.getByTitle("Toggle theme");
    expect(themeBtn).toBeInTheDocument();
  });

  it("renders ProfileMenu (avatar initials)", () => {
    renderTopbar("/");
    // Default mock returns no user, so initials show "?"
    // The "?" text appears in both the avatar button and the large avatar in the menu
    expect(screen.getAllByText("?").length).toBeGreaterThanOrEqual(1);
  });
});
