import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../context/ThemeContext";
import { ToastProvider } from "./Toast";
import { ProfileMenu } from "./ProfileMenu";

// Mock useNavigate
const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

// Mock useAuth to control user / auth state
const mockLogout = vi.fn();
const authState: any = {
  user: null,
  isLoading: false,
  isAuthenticated: false,
  permissions: undefined,
  authMode: "local" as const,
  login: vi.fn(),
  logout: mockLogout,
};

vi.mock("../context/AuthContext", () => ({
  useAuth: vi.fn(() => authState),
  AuthProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));


function renderProfileMenu() {
  return render(
    <MemoryRouter>
      <ThemeProvider>
        <ToastProvider>
          <ProfileMenu />
        </ToastProvider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

/** Helper to grab the avatar button by text content */
function getAvatarBtn() {
  // The dropdown menu content is always in the DOM but hidden via CSS.
  // The avatar button appears FIRST in DOM order; the large avatar (inside the menu) is later.
  const items = screen.getAllByText(
    (content) => content === "?" || content === "…" || content === "JD" || content === "JS",
  );
  // The first matching element is the avatar <button> (appears before the menu in the DOM)
  return items[0].closest("button");
}

/** Helper to grab the menu div (sibling of the avatar button) */
function getMenuDiv() {
  const avatarBtn = getAvatarBtn();
  return avatarBtn?.nextElementSibling as HTMLElement | null;
}

describe("ProfileMenu", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockLogout.mockReset();
    mockNavigate.mockReset();
    // Reset auth state to defaults
    Object.assign(authState, {
      user: null,
      isLoading: false,
      isAuthenticated: false,
      permissions: undefined,
      authMode: "local",
      login: vi.fn(),
      logout: mockLogout,
    });
  });

  describe("avatar / initials", () => {
    it("renders '?' when no user is provided", () => {
      renderProfileMenu();
      // Both the avatar button and large avatar in menu show "?"
      const questionMarks = screen.getAllByText("?");
      expect(questionMarks.length).toBeGreaterThanOrEqual(1);
    });

    it("renders user initials from displayName", () => {
      authState.user = {
        displayName: "John Doe",
        email: "john@example.com",
        name: "John Doe",
        preferredUsername: "jdoe",
        subject: "user:jdoe",
        groups: [],
      };
      renderProfileMenu();
      // Shows "JD" in both avatar button and large avatar
      const initials = screen.getAllByText("JD");
      expect(initials.length).toBeGreaterThanOrEqual(1);
    });

    it("renders initials from name when displayName is absent", () => {
      authState.user = {
        name: "Jane Smith",
        email: "jane@example.com",
        preferredUsername: "jsmith",
        subject: "user:jsmith",
        groups: [],
      };
      renderProfileMenu();
      const initials = screen.getAllByText("JS");
      expect(initials.length).toBeGreaterThanOrEqual(1);
    });

    it("renders '…' when loading", () => {
      authState.isLoading = true;
      renderProfileMenu();
      const ellipses = screen.getAllByText("…");
      expect(ellipses.length).toBeGreaterThanOrEqual(1);
    });
  });

  describe("dropdown open/close via CSS class", () => {
    it("has closed class initially", () => {
      renderProfileMenu();
      const menuDiv = getMenuDiv();
      expect(menuDiv).toBeTruthy();
      // The open class is NOT present when dropdown is closed
      expect(menuDiv!.className).not.toContain("open");
    });

    it("toggles open class when avatar is clicked", async () => {
      renderProfileMenu();
      const menuDiv = getMenuDiv();
      expect(menuDiv!.className).not.toContain("open");

      // Open by clicking avatar
      await userEvent.click(getAvatarBtn()!);
      expect(menuDiv!.className).toContain("open");

      // Close by clicking avatar again
      await userEvent.click(getAvatarBtn()!);
      expect(menuDiv!.className).not.toContain("open");
    });
  });

  describe("dropdown content (always in DOM, hidden via CSS)", () => {
    it("shows user info (name and email) in the menu", () => {
      authState.user = {
        displayName: "John Doe",
        email: "john@example.com",
        name: "John Doe",
        preferredUsername: "jdoe",
        subject: "user:jdoe",
        groups: [],
      };
      renderProfileMenu();
      expect(screen.getByText("John Doe")).toBeInTheDocument();
      expect(screen.getByText("john@example.com")).toBeInTheDocument();
    });

    it("shows 'Sign in' when no user name and 'Not authenticated' when no email", () => {
      renderProfileMenu();
      expect(screen.getByText("Sign in")).toBeInTheDocument();
      expect(screen.getByText("Not authenticated")).toBeInTheDocument();
    });

    it("shows 'Loading…' in dropdown when auth is loading", () => {
      authState.isLoading = true;
      renderProfileMenu();
      expect(screen.getByText("Loading…")).toBeInTheDocument();
    });

    it("renders profile and API Keys without Preferences", () => {
      renderProfileMenu();
      expect(screen.getByText("My profile")).toBeInTheDocument();
      expect(screen.getByText("API Keys")).toBeInTheDocument();
      expect(screen.queryByText("Preferences")).not.toBeInTheDocument();
    });
  });

  describe("navigation", () => {
    it("navigates to /profile when 'My profile' is clicked", async () => {
      renderProfileMenu();
      await userEvent.click(screen.getByText("My profile"));
      expect(mockNavigate).toHaveBeenCalledWith("/profile");
    });

    it("navigates to the standalone API Keys page", async () => {
      renderProfileMenu();
      await userEvent.click(screen.getByText("API Keys"));
      expect(mockNavigate).toHaveBeenCalledWith("/api-keys");
    });
  });

  describe("theme toggle", () => {
    it("renders Light and Dark theme buttons", () => {
      renderProfileMenu();
      expect(screen.getByText("Light")).toBeInTheDocument();
      expect(screen.getByText("Dark")).toBeInTheDocument();
    });
  });

  describe("accent options", () => {
    it("renders accent color swatches", () => {
      renderProfileMenu();
      expect(screen.getByTitle("Honey")).toBeInTheDocument();
      expect(screen.getByTitle("Carapace")).toBeInTheDocument();
      expect(screen.getByTitle("Pollen")).toBeInTheDocument();
      expect(screen.getByTitle("Propolis")).toBeInTheDocument();
    });
  });

  describe("logout", () => {
    it("calls logout from useAuth when 'Log out' is clicked", async () => {
      renderProfileMenu();
      await userEvent.click(screen.getByText(/Log out/));
      expect(mockLogout).toHaveBeenCalledTimes(1);
    });
  });
});
