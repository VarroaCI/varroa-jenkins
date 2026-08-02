import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../context/ThemeContext";
import { ToastProvider } from "./Toast";
import { AuthProvider } from "../context/AuthContext";
import { ComposerProvider } from "../context/ComposerContext";
import { createTestQueryClient } from "../test/render-utils";
import Layout from "./Layout";

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  localStorage.clear();
});

function renderLayout(route = "/") {
  const queryClient = createTestQueryClient();

  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <ToastProvider>
            <AuthProvider>
              <ComposerProvider>
                <Routes>
                  <Route element={<Layout />}>
                    <Route path="/" element={<div data-testid="outlet-content">Home Content</div>} />
                    <Route path="test-route" element={<div data-testid="outlet-content">Child Content</div>} />
                  </Route>
                </Routes>
              </ComposerProvider>
            </AuthProvider>
          </ToastProvider>
        </ThemeProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

describe("Layout", () => {
  it("renders Sidebar with brand name", () => {
    renderLayout("/");
    expect(screen.getByText("Varroa")).toBeInTheDocument();
    expect(screen.getByText("Jenkins control plane")).toBeInTheDocument();
  });

  it("renders Topbar with breadcrumbs for the current route", () => {
    renderLayout("/test-route");
    // The breadcrumb capitalized from the first path segment
    expect(screen.getByText("Test-route")).toBeInTheDocument();
  });

  it("renders child content via Outlet", () => {
    renderLayout("/");
    expect(screen.getByTestId("outlet-content")).toHaveTextContent("Home Content");
  });

  it("renders child content for nested routes", () => {
    renderLayout("/test-route");
    expect(screen.getByTestId("outlet-content")).toHaveTextContent("Child Content");
  });

  it("renders CommandPalette (keyboard shortcut hint visible in Topbar)", () => {
    renderLayout("/");
    // The Topbar shows "⌘K" as a keyboard shortcut hint
    expect(screen.getByText("⌘K")).toBeInTheDocument();
  });

  it("wraps content in a Layout container element", () => {
    const { container } = renderLayout("/");
    // The Layout root div.app is the first element child of the container.
    // It contains the Sidebar (aside), Topbar (header), and Outlet content.
    const rootEl = container.firstElementChild;
    expect(rootEl).toBeInTheDocument();
    expect(rootEl!.tagName).toBe("DIV");
    // Sidebar, Topbar, and Outlet should be descendants of the Layout root
    expect(rootEl!.querySelector("aside")).toBeInTheDocument();
    expect(rootEl!.querySelector("header")).toBeInTheDocument();
  });

  describe("sidebar collapse", () => {
    it("defaults to expanded when no localStorage key is set", () => {
      renderLayout("/");
      const toggle = screen.getByRole("button", { name: "Collapse sidebar" });
      expect(toggle).toHaveAttribute("aria-expanded", "true");
    });

    it("pre-sets collapsed when localStorage key is 'true'", () => {
      localStorage.setItem("varroa-sidebar-collapsed", "true");
      renderLayout("/");
      const toggle = screen.getByRole("button", { name: "Expand sidebar" });
      expect(toggle).toHaveAttribute("aria-expanded", "false");
    });

    it("toggles on button click and persists to localStorage", async () => {
      const user = userEvent.setup();
      renderLayout("/");

      const toggle = screen.getByRole("button", { name: "Collapse sidebar" });
      await user.click(toggle);

      // aria-expanded flips
      expect(toggle).toHaveAttribute("aria-expanded", "false");
      expect(toggle).toHaveAttribute("aria-label", "Expand sidebar");
      // localStorage written
      expect(localStorage.getItem("varroa-sidebar-collapsed")).toBe("true");

      // click again
      await user.click(toggle);
      expect(toggle).toHaveAttribute("aria-expanded", "true");
      expect(toggle).toHaveAttribute("aria-label", "Collapse sidebar");
      expect(localStorage.getItem("varroa-sidebar-collapsed")).toBe("false");
    });

    it("toggles on pressing [", () => {
      renderLayout("/");

      expect(screen.getByRole("button", { name: "Collapse sidebar" })).toHaveAttribute("aria-expanded", "true");

      fireEvent.keyDown(document.body, { key: "[" });

      expect(screen.getByRole("button", { name: "Expand sidebar" })).toHaveAttribute("aria-expanded", "false");
      expect(localStorage.getItem("varroa-sidebar-collapsed")).toBe("true");
    });

    it("does NOT toggle on pressing [ when focus is in an input", async () => {
      const user = userEvent.setup();
      renderLayout("/");

      const searchInput = screen.getByRole("textbox", { name: "Global search" });
      await user.click(searchInput);
      fireEvent.keyDown(searchInput, { key: "[" });

      // sidebar should still be expanded
      expect(screen.getByRole("button", { name: "Collapse sidebar" })).toHaveAttribute("aria-expanded", "true");
    });

    it("does NOT toggle on pressing [ when the palette is open", async () => {
      const user = userEvent.setup();
      renderLayout("/");

      // Open palette with ⌘K
      await user.keyboard("{Meta>}k{/Meta}");

      // Press [ — should not toggle because palette is open
      fireEvent.keyDown(document.body, { key: "[" });

      expect(screen.getByRole("button", { name: "Collapse sidebar" })).toHaveAttribute("aria-expanded", "true");
    });
  });
});
