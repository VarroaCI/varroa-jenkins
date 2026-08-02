import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import { useAuth } from "./AuthContext";
import { createMeResponse } from "../test/factories";

// ---- Mocks ----

const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  logout: vi.fn().mockResolvedValue(undefined),
}));

// ---- Test consumer component ----

function AuthTestComponent() {
  const auth = useAuth();
  return (
    <div>
      <span data-testid="loading">{String(auth.isLoading)}</span>
      <span data-testid="authenticated">{String(auth.isAuthenticated)}</span>
      <span data-testid="user-name">{auth.user?.name ?? "null"}</span>
      <span data-testid="auth-mode">{auth.authMode ?? "null"}</span>
      <button data-testid="login-btn" onClick={() => auth.login("testuser", "testpass")}>
        Login
      </button>
      <button data-testid="logout-btn" onClick={() => auth.logout()}>
        Logout
      </button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  mockBffFetch.mockReset();
});

// ---- Tests ----

describe("AuthContext", () => {
  it("provides isLoading: true initially when /me is pending", async () => {
    // Keep /me unresolved so isLoading stays true.
    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/auth-config") return Promise.resolve({ mode: "local" });
      if (path === "/me") return new Promise(() => {}); // never settles
      return Promise.resolve({});
    });

    renderWithProviders(<AuthTestComponent />);

    await waitFor(() => {
      expect(screen.getByTestId("loading")).toHaveTextContent("true");
    });
  });

  it("provides user data once /me resolves", async () => {
    const user = createMeResponse({ name: "Test User", email: "test@example.com" });
    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/auth-config") return Promise.resolve({ mode: "local" });
      if (path === "/me") return Promise.resolve(user);
      return Promise.resolve({});
    });

    renderWithProviders(<AuthTestComponent />);

    await waitFor(() => {
      expect(screen.getByTestId("loading")).toHaveTextContent("false");
    });
    expect(screen.getByTestId("user-name")).toHaveTextContent("Test User");
    expect(screen.getByTestId("authenticated")).toHaveTextContent("true");
  });

  it("sets isAuthenticated to false when user is null", async () => {
    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/auth-config") return Promise.resolve({ mode: "local" });
      if (path === "/me") return Promise.resolve(null);
      return Promise.resolve({});
    });

    renderWithProviders(<AuthTestComponent />);

    await waitFor(() => {
      expect(screen.getByTestId("loading")).toHaveTextContent("false");
    });
    expect(screen.getByTestId("authenticated")).toHaveTextContent("false");
    expect(screen.getByTestId("user-name")).toHaveTextContent("null");
  });

  it("reads authMode from /auth-config", async () => {
    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/auth-config") return Promise.resolve({ mode: "oidc" });
      if (path === "/me") return Promise.resolve(createMeResponse());
      return Promise.resolve({});
    });

    renderWithProviders(<AuthTestComponent />);

    await waitFor(() => {
      expect(screen.getByTestId("auth-mode")).toHaveTextContent("oidc");
    });
  });

  it("login() with local auth calls bffFetch and stores token/user in localStorage", async () => {
    const user = userEvent.setup();

    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/auth-config") return Promise.resolve({ mode: "local" });
      if (path === "/me") return Promise.resolve(createMeResponse({ name: "Test User" }));
      if (path === "/login") return Promise.resolve({ id_token: "token123", expires_in: 3600 });
      return Promise.resolve({});
    });

    renderWithProviders(<AuthTestComponent />);

    // Wait for auth config to be available (login checks authConfig?.mode).
    await waitFor(() => {
      expect(screen.getByTestId("auth-mode")).toHaveTextContent("local");
    });

    await user.click(screen.getByTestId("login-btn"));

    // Login calls bffFetch with POST and credentials.
    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalledWith(
        "/login",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ username: "testuser", password: "testpass" }),
        }),
      );
    });

    expect(localStorage.getItem("varroa_id_token")).toBe("token123");
    expect(localStorage.getItem("varroa_user")).toBe("testuser");
  });

  it("logout() clears localStorage and sets me query data to null", async () => {
    const user = userEvent.setup();

    mockBffFetch.mockImplementation((path: string) => {
      if (path === "/auth-config") return Promise.resolve({ mode: "local" });
      if (path === "/me") return Promise.resolve(createMeResponse({ name: "Test User" }));
      if (path === "/logout") return Promise.resolve(undefined);
      return Promise.resolve({});
    });

    // Pre-populate localStorage as if already logged in.
    localStorage.setItem("varroa_id_token", "old-token");
    localStorage.setItem("varroa_user", "testuser");

    const { queryClient } = renderWithProviders(<AuthTestComponent />);

    // Wait for auth to be ready (isAuthenticated).
    await waitFor(() => {
      expect(screen.getByTestId("authenticated")).toHaveTextContent("true");
    });

    await user.click(screen.getByTestId("logout-btn"));

    // localStorage should be cleared.
    await waitFor(() => {
      expect(localStorage.getItem("varroa_id_token")).toBeNull();
      expect(localStorage.getItem("varroa_user")).toBeNull();
    });

    // "me" query data should be null.
    expect(queryClient.getQueryData(["me"])).toBeNull();
  });
});
