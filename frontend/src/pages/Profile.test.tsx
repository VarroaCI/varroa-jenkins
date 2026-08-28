import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

// Mock useApi
const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
}));

// Mock AuthContext
const mockAuthLogout = vi.fn();
const mockLogin = vi.fn();
vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({
    user: null,
    phase: "authenticated" as const,
    isLoading: false,
    isAuthenticated: true,
    permissions: undefined,
    authMode: "local" as const,
    login: mockLogin,
    logout: mockAuthLogout,
    authError: undefined,
  }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  PROGRESS_COPY: {
    loadingConfig: "Preparing secure sign-in",
    checkingSession: "Checking your session",
    redirecting: "Redirecting to sign in",
    callback: "Signing you in",
  },
}));

// Mock ThemeContext — use vi.hoisted so tests can mutate theme values
const mockSetTheme = vi.fn();
const mockSetAccent = vi.fn();
const themeState = vi.hoisted(() => ({
  theme: "light" as string,
  accent: "honey" as string,
}));
vi.mock("../context/ThemeContext", () => ({
  useTheme: () => ({
    theme: themeState.theme,
    accent: themeState.accent,
    setTheme: mockSetTheme,
    setAccent: mockSetAccent,
  }),
}));

// Mock Toast
const mockToast = vi.fn();
vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: mockToast }),
}));

// Mock Card
vi.mock("../components/Card", () => ({
  Card: ({ title, children }: { title: string; children: React.ReactNode }) => (
    <div data-testid="card">
      <h3>{title}</h3>
      {children}
    </div>
  ),
}));

// Mock KVGrid
vi.mock("../components/KVGrid", () => ({
  KVGrid: ({ items }: { items: { key: string; value: string }[] }) => (
    <div data-testid="kvgrid">
      {items.map((item) => (
        <div key={item.key}>
          <span>{item.key}</span>
          <span>{item.value}</span>
        </div>
      ))}
    </div>
  ),
}));

// Mock Button
vi.mock("../components/Button", () => ({
  Button: ({ children, onClick, variant, ...props }: Record<string, unknown>) => (
    <button onClick={onClick as () => void} data-variant={variant as string} {...props}>
      {children as string}
    </button>
  ),
}));

import Profile from "./Profile";

function renderProfile() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <Profile />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockBffFetch.mockReset();
  mockAuthLogout.mockReset();
  mockToast.mockReset();
  mockSetTheme.mockReset();
  mockSetAccent.mockReset();
});

describe("Profile password change (local mode)", () => {
  const localMe = {
    subject: "s", preferredUsername: "u", email: "u@example.com",
    name: "U", displayName: "U", groups: [], authMode: "local",
  };
  const pwCall = () => mockBffFetch.mock.calls.find((c) => c[0] === "/me/password");

  it("submits a valid password change", async () => {
    mockBffFetch.mockResolvedValue(localMe);
    renderProfile();

    await screen.findByText("🔑 Change password");
    await userEvent.type(screen.getByPlaceholderText("Current password"), "oldpass12");
    await userEvent.type(screen.getByPlaceholderText("New password (min 8 chars)"), "newpass12");
    await userEvent.type(screen.getByPlaceholderText("Confirm new password"), "newpass12");
    await userEvent.click(screen.getByText("Change password"));

    await waitFor(() => expect(pwCall()).toBeTruthy());
    expect(JSON.parse(pwCall()![1].body)).toEqual({ oldPassword: "oldpass12", newPassword: "newpass12" });
  });

  it("rejects a mismatched confirmation without calling the API", async () => {
    mockBffFetch.mockResolvedValue(localMe);
    renderProfile();

    await screen.findByText("🔑 Change password");
    await userEvent.type(screen.getByPlaceholderText("Current password"), "oldpass12");
    await userEvent.type(screen.getByPlaceholderText("New password (min 8 chars)"), "newpass12");
    await userEvent.type(screen.getByPlaceholderText("Confirm new password"), "different12");
    await userEvent.click(screen.getByText("Change password"));

    await screen.findByText("Passwords do not match");
    expect(pwCall()).toBeFalsy();
  });

  it("hides the password form in OIDC mode", async () => {
    mockBffFetch.mockResolvedValue({ ...localMe, authMode: "oidc" });
    renderProfile();
    await screen.findByText("My profile");
    expect(screen.queryByText("🔑 Change password")).not.toBeInTheDocument();
  });
});

describe("Profile page", () => {
  it("shows loading state", () => {
    mockBffFetch.mockReturnValue(new Promise(() => {}));
    renderProfile();
    expect(screen.getByText("Loading profile...")).toBeInTheDocument();
  });

  it("renders account information after loading", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: ["developers"],
    });
    renderProfile();

    await screen.findByText("My profile");
    // "Test User" appears in multiple places (title, avatar, card)
    expect(screen.getAllByText("Test User").length).toBeGreaterThanOrEqual(1);
  });

  it("renders initials from display name", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:jane@example.com",
      preferredUsername: "jane",
      email: "jane@example.com",
      name: "Jane Doe",
      displayName: "Jane Doe",
      groups: ["users"],
    });
    renderProfile();

    await screen.findByText("JD");
  });

  it("renders initials from name when displayName is missing", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:john@example.com",
      preferredUsername: "john",
      email: "john@example.com",
      name: "John Smith",
      groups: ["users"],
    });
    renderProfile();

    await screen.findByText("JS");
  });

  it("renders fallback 'U' initials when no name", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:unknown@example.com",
      preferredUsername: "unknown",
      email: "unknown@example.com",
      name: "",
      groups: [],
    });
    renderProfile();

    await screen.findByText("U");
  });

  it("shows user info in account card", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: ["developers"],
    });
    renderProfile();

    await screen.findByText("testuser");
    // email appears in multiple places (subject field, email field)
    expect(screen.getAllByText("test@example.com").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(/developers/)).toBeInTheDocument();
  });

  it("toggles theme to dark", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    renderProfile();

    await screen.findByText("Dark");
    await userEvent.click(screen.getByText("Dark"));

    expect(mockSetTheme).toHaveBeenCalledWith("dark");
  });

  it("toggles theme to light", async () => {
    // Override theme to dark via mutable themeState
    themeState.theme = "dark";

    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    renderProfile();

    await screen.findByText("Light");
    await userEvent.click(screen.getByText("Light"));
    expect(mockSetTheme).toHaveBeenCalledWith("light");

    // Reset
    themeState.theme = "light";
  });

  it("changes accent color", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    renderProfile();

    // Find the rust/burnt orange swatch
    const rustButton = await screen.findByTitle("Carapace");
    await userEvent.click(rustButton);

    expect(mockSetAccent).toHaveBeenCalledWith("rust");
  });

  it("logs out and shows toast", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    mockAuthLogout.mockResolvedValue(undefined);
    renderProfile();

    const logoutBtn = await screen.findByText(/Log out/);
    await userEvent.click(logoutBtn);

    expect(mockAuthLogout).toHaveBeenCalled();
    await waitFor(() => {
      expect(mockToast).toHaveBeenCalledWith(
        expect.stringContaining("Session cleared"),
      );
    });
  });

  it("shows toast on logout failure", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    mockAuthLogout.mockRejectedValue(new Error("Failed"));
    renderProfile();

    const logoutBtn = await screen.findByText(/Log out/);
    await userEvent.click(logoutBtn);

    await waitFor(() => {
      expect(mockToast).toHaveBeenCalledWith("Logout failed");
    });
  });

  it("renders accent color swatches", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    renderProfile();

    await screen.findByTitle("Honey");
    expect(screen.getByTitle("Carapace")).toBeInTheDocument();
    expect(screen.getByTitle("Pollen")).toBeInTheDocument();
    expect(screen.getByTitle("Propolis")).toBeInTheDocument();
  });

  it("renders 'My profile' heading", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    renderProfile();
    await screen.findByText("My profile");
    expect(screen.queryByRole("heading", { name: /API Keys/i })).not.toBeInTheDocument();
  });

  it("shows saving indicator when preferences change", async () => {
    mockBffFetch.mockResolvedValue({
      subject: "user:test@example.com",
      preferredUsername: "testuser",
      email: "test@example.com",
      name: "Test User",
      displayName: "Test User",
      groups: [],
    });
    // Don't resolve the preferences PUT
    mockBffFetch.mockImplementation((url: string) => {
      if (url === "/me") {
        return Promise.resolve({
          subject: "user:test@example.com",
          preferredUsername: "testuser",
          email: "test@example.com",
          name: "Test User",
          displayName: "Test User",
          groups: [],
        });
      }
      return new Promise(() => {});
    });
    renderProfile();

    await screen.findByText("Dark");
    await userEvent.click(screen.getByText("Dark"));

    await screen.findByText("Saving...");
  });
});
