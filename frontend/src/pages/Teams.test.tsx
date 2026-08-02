import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Teams from "./Teams";
import { renderWithProviders } from "../test/render-utils";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListTeams = vi.fn();
const mockDeleteTeam = vi.fn();
vi.mock("../api/client", () => ({
  listTeams: (...args: unknown[]) => mockListTeams(...args),
  deleteTeam: (...args: unknown[]) => mockDeleteTeam(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: { "*": { "*": true } }, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const team1 = {
  name: "alpha-team",
  members: ["alice"],
  namespaces: ["ns-alpha"],
  roleRef: "developer",
  conditions: [{ type: "TeamReady", status: "True" }],
};

const team2 = {
  name: "beta-team",
  subjects: [{ kind: "Group", name: "idp-devs" }],
  namespaces: ["ns-beta"],
  roleRef: "viewer",
  conditions: [{ type: "TeamReady", status: "False", reason: "NamespaceMissing" }],
};

describe("Teams", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListTeams.mockResolvedValue([team1, team2]);
  });

  it("shows loading state", () => {
    mockListTeams.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<Teams />);
    expect(screen.getByText("Loading...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListTeams.mockRejectedValue(new Error("Server error"));
    renderWithProviders(<Teams />);
    await waitFor(() => {
      expect(screen.getByText(/Server error/)).toBeInTheDocument();
    });
  });

  it("renders team list", async () => {
    renderWithProviders(<Teams />);
    await waitFor(() => {
      expect(screen.getByText("alpha-team")).toBeInTheDocument();
    });
    expect(screen.getByText("beta-team")).toBeInTheDocument();
  });

  it("shows status badges", async () => {
    renderWithProviders(<Teams />);
    await waitFor(() => {
      expect(screen.getByText("Ready")).toBeInTheDocument();
    });
    expect(screen.getByText("NamespaceMissing")).toBeInTheDocument();
  });

  it("shows create button for admin", async () => {
    renderWithProviders(<Teams />);
    await waitFor(() => {
      expect(screen.getByText("Create Team")).toBeInTheDocument();
    });
  });

  it("navigates to create form", async () => {
    renderWithProviders(<Teams />);
    await waitFor(() => {
      screen.getByText("Create Team").click();
    });
    expect(mockNavigate).toHaveBeenCalledWith("/access/teams/create");
  });

  it("can delete a team", async () => {
    mockDeleteTeam.mockResolvedValue(undefined);
    renderWithProviders(<Teams />);
    await waitFor(() => {
      expect(screen.getByText("alpha-team")).toBeInTheDocument();
    });

    const deleteButtons = screen.getAllByText("Delete");
    await userEvent.click(deleteButtons[0]);

    await waitFor(() => {
      expect(screen.getByText(/Delete team "alpha-team"/)).toBeInTheDocument();
    });

    const confirmButtons = screen.getAllByText("Delete");
    // Click the Delete button in the modal (the second "Delete" instance)
    await userEvent.click(confirmButtons[confirmButtons.length - 1]);
    await waitFor(() => {
      expect(mockDeleteTeam).toHaveBeenCalledWith("alpha-team");
    });
  });
});
