import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { ProtectedRoute } from "./ProtectedRoute";

// Mock the entire auth context module so we control useAuth in every test
vi.mock("../context/AuthContext", () => ({
  useAuth: vi.fn(),
  PROGRESS_COPY: {
    loadingConfig: "Preparing secure sign-in",
    checkingSession: "Checking your session",
    redirecting: "Redirecting to sign in",
    callback: "Signing you in",
  },
  AuthProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { useAuth } from "../context/AuthContext";

function renderProtectedRoute(
  authOverrides: Partial<ReturnType<typeof useAuth>> = {},
) {
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

  vi.mocked(useAuth).mockReturnValue({ ...defaults, ...authOverrides });

  return render(
    <MemoryRouter initialEntries={["/test-route"]}>
      <Routes>
        <Route element={<ProtectedRoute />}>
          <Route path="test-route" element={<div data-testid="protected-content">Protected Content</div>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("ProtectedRoute", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows progress copy when loadingConfig", () => {
    renderProtectedRoute({ phase: "loadingConfig" });
    expect(screen.getByText("Preparing secure sign-in")).toBeInTheDocument();
  });

  it("shows progress copy when checkingSession", () => {
    renderProtectedRoute({ phase: "checkingSession", authMode: "oidc" });
    expect(screen.getByText("Checking your session")).toBeInTheDocument();
  });

  it("shows progress copy when redirecting", () => {
    renderProtectedRoute({ phase: "redirecting", authMode: "oidc" });
    expect(screen.getByText("Redirecting to sign in")).toBeInTheDocument();
  });

  it("shows progress copy when callback", () => {
    renderProtectedRoute({ phase: "callback", authMode: "oidc" });
    expect(screen.getByText("Signing you in")).toBeInTheDocument();
  });

  it("renders LoginPage when phase is loggedOut with OIDC", () => {
    renderProtectedRoute({ phase: "loggedOut", isAuthenticated: false, authMode: "oidc" });
    expect(screen.getByText("Sign in to Varroa")).toBeInTheDocument();
    // OIDC mode shows SSO button, not username/password fields
    expect(screen.getByText("Sign in with SSO")).toBeInTheDocument();
  });

  it("does not render protected content when logged out with OIDC", () => {
    renderProtectedRoute({ phase: "loggedOut", isAuthenticated: false, authMode: "oidc" });
    expect(screen.queryByTestId("protected-content")).not.toBeInTheDocument();
  });

  it("renders LoginPage with error when phase is error", () => {
    renderProtectedRoute({ phase: "error", authError: "access_denied", isAuthenticated: false, authMode: "oidc" });
    expect(screen.getByText(/Authentication failed/)).toBeInTheDocument();
  });

  it("renders LoginPage when local mode and not authenticated", () => {
    renderProtectedRoute({ phase: "checkingSession", isAuthenticated: false, authMode: "local" });
    expect(screen.getByText("Sign in to Varroa")).toBeInTheDocument();
    // Local mode shows form fields
    expect(screen.getByLabelText("Username")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
  });

  it("does not redirect to /login automatically when logged out with OIDC", () => {
    const locationMock = { href: "" };
    vi.stubGlobal("location", locationMock);
    renderProtectedRoute({ phase: "loggedOut", isAuthenticated: false, authMode: "oidc" });
    // The new ProtectedRoute does NOT auto-redirect for OIDC
    expect(locationMock.href).toBe("");
    vi.unstubAllGlobals();
  });

  it("renders Outlet (child route content) when authenticated", () => {
    renderProtectedRoute({ phase: "authenticated", isAuthenticated: true, authMode: "local" });
    expect(screen.getByTestId("protected-content")).toHaveTextContent("Protected Content");
  });

  it("renders Outlet when authenticated regardless of authMode", () => {
    renderProtectedRoute({ phase: "authenticated", isAuthenticated: true, authMode: "oidc" });
    expect(screen.getByTestId("protected-content")).toHaveTextContent("Protected Content");
  });
});
