import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import type { ReactElement } from "react";
import { render as rtlRender, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import CliAuth from "./CliAuth";

// Mock useAuth so we control auth state in every test.
vi.mock("../context/AuthContext", () => ({
  useAuth: vi.fn(),
  AuthProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { useAuth } from "../context/AuthContext";

const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
}));

function renderWithAuth(overrideAuth: Partial<ReturnType<typeof useAuth>> = {}) {
  const defaults: ReturnType<typeof useAuth> = {
    user: null,
    phase: "loadingConfig",
    isLoading: false,
    isAuthenticated: false,
    permissions: undefined,
    authMode: "local",
    login: vi.fn(),
    logout: vi.fn(),
    authError: undefined,
  };
  vi.mocked(useAuth).mockReturnValue({ ...defaults, ...overrideAuth });

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui: ReactElement = (
    <QueryClientProvider client={queryClient}>
      <CliAuth />
    </QueryClientProvider>
  );
  return { queryClient, ...rtlRender(ui) };
}

/** Set up a mock window.location with the given search string. */
function mockLocation(search: string) {
  vi.stubGlobal("location", {
    href: "",
    pathname: "/cli-auth",
    search,
    replace: vi.fn(),
  });
}

describe("CliAuth", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("shows error card for invalid port (0) and does not redirect", () => {
    mockLocation("?port=0&state=abc");
    renderWithAuth();
    expect(screen.getByText("Invalid CLI Login Request")).toBeInTheDocument();
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
    expect(loc.replace).not.toHaveBeenCalled();
  });

  it("shows error card for port > 65535 and does not redirect", () => {
    mockLocation("?port=99999&state=abc");
    renderWithAuth();
    expect(screen.getByText("Invalid CLI Login Request")).toBeInTheDocument();
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
    expect(loc.replace).not.toHaveBeenCalled();
  });

  it("shows error card for missing state and does not redirect", () => {
    mockLocation("?port=12345");
    renderWithAuth();
    expect(screen.getByText("Invalid CLI Login Request")).toBeInTheDocument();
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
    expect(loc.replace).not.toHaveBeenCalled();
  });

  it("shows error card for missing port and state and does not redirect", () => {
    mockLocation("");
    renderWithAuth();
    expect(screen.getByText("Invalid CLI Login Request")).toBeInTheDocument();
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
    expect(loc.replace).not.toHaveBeenCalled();
  });

  it("redirects to /login with encoded state for unauth oidc", () => {
    mockLocation("?port=12345&state=abc123");
    renderWithAuth({ isAuthenticated: false, authMode: "oidc", isLoading: false });
    const loc = (globalThis as any).location;
    expect(loc.href).toBe(
      "/login?state=" + encodeURIComponent("/cli-auth?port=12345&state=abc123"),
    );
  });

  it("does not redirect when auth config is still loading (oidc)", () => {
    mockLocation("?port=12345&state=abc123");
    renderWithAuth({ isAuthenticated: false, authMode: "oidc", isLoading: true });
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
  });

  it("shows inline login form for unauth local auth", () => {
    mockLocation("?port=12345&state=abc123");
    renderWithAuth({ isAuthenticated: false, authMode: "local", isLoading: false });
    expect(screen.getByText(/Sign in to authorize the CLI/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Username")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
    expect(screen.getByText("Sign in")).toBeInTheDocument();
    // No navigation.
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
    expect(loc.replace).not.toHaveBeenCalled();
  });

  it("inline login posts /login, stores tokens, and does not navigate", async () => {
    mockLocation("?port=12345&state=abc123");
    mockBffFetch.mockResolvedValue({ id_token: "jwt123", expires_in: 3600 });
    renderWithAuth({ isAuthenticated: false, authMode: "local", isLoading: false });

    fireEvent.change(screen.getByPlaceholderText("Username"), { target: { value: "alice" } });
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "secret" } });
    fireEvent.click(screen.getByText("Sign in"));

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalledWith("/login", {
        method: "POST",
        body: JSON.stringify({ username: "alice", password: "secret" }),
      });
    });
    expect(localStorage.getItem("varroa_id_token")).toBe("jwt123");
    expect(localStorage.getItem("varroa_user")).toBe("alice");
    // No navigation.
    const loc = (globalThis as any).location;
    expect(loc.href).toBe("");
    expect(loc.replace).not.toHaveBeenCalled();
  });

  it("shows confirm card when authenticated with name from query param", () => {
    mockLocation("?port=12345&state=abc123&name=varroactl%40box");
    renderWithAuth({
      isAuthenticated: true,
      authMode: "local",
      isLoading: false,
      user: { email: "alice@example.com" } as any,
    });
    expect(screen.getByText(/127.0.0.1:12345/)).toBeInTheDocument();
    expect(screen.getByText(/varroactl@box/)).toBeInTheDocument();
    expect(screen.getByText(/alice@example.com/)).toBeInTheDocument();
    expect(screen.getByText("Approve")).toBeInTheDocument();
    expect(screen.getByText("Deny")).toBeInTheDocument();
  });

  it("approve posts /me/apikeys and redirects to callback with token", async () => {
    mockLocation("?port=12345&state=abc123&name=varroactl%40box");
    mockBffFetch.mockResolvedValue({ token: "vk_test.secret" });
    renderWithAuth({
      isAuthenticated: true,
      authMode: "local",
      isLoading: false,
      user: { email: "alice@example.com" } as any,
    });

    fireEvent.click(screen.getByText("Approve"));

    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalledWith("/me/apikeys", {
        method: "POST",
        body: JSON.stringify({ name: "varroactl@box" }),
      });
    });
    const loc = (globalThis as any).location;
    expect(loc.replace).toHaveBeenCalledWith(
      "http://127.0.0.1:12345/callback?state=abc123&token=vk_test.secret",
    );
  });

  it("deny redirects to callback with error=denied", () => {
    mockLocation("?port=12345&state=abc123");
    renderWithAuth({
      isAuthenticated: true,
      authMode: "local",
      isLoading: false,
      user: { email: "alice@example.com" } as any,
    });

    fireEvent.click(screen.getByText("Deny"));

    const loc = (globalThis as any).location;
    expect(loc.replace).toHaveBeenCalledWith(
      "http://127.0.0.1:12345/callback?state=abc123&error=denied",
    );
  });

  it("shows error card when key mint fails and does not redirect", async () => {
    mockLocation("?port=12345&state=abc123");
    mockBffFetch.mockRejectedValue(new Error("something went wrong"));
    renderWithAuth({
      isAuthenticated: true,
      authMode: "local",
      isLoading: false,
      user: { email: "alice@example.com" } as any,
    });

    fireEvent.click(screen.getByText("Approve"));

    await waitFor(() => {
      expect(screen.getByText("something went wrong")).toBeInTheDocument();
    });
    const loc = (globalThis as any).location;
    expect(loc.replace).not.toHaveBeenCalled();
  });
});
