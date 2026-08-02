import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoles from "./JenkinsRoles";
import { renderWithProviders } from "../test/render-utils";
import { createJenkinsRole } from "../test/factories";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListJenkinsRoles = vi.fn();
const mockDeleteJenkinsRole = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  listJenkinsRoles: (...args: unknown[]) => mockListJenkinsRoles(...args),
  deleteJenkinsRole: (...args: unknown[]) => mockDeleteJenkinsRole(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: () => true,
}));

const globalRole = createJenkinsRole({
  metadata: { name: "jenkins-global-reader" },
  spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Read"], description: "Can read everything" },
});

const itemRole = createJenkinsRole({
  metadata: { name: "jenkins-item-builder" },
  spec: { roleType: "Item", permissions: ["hudson.model.Item.Build"], description: "" },
});

describe("JenkinsRoles", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListJenkinsRoles.mockResolvedValue({ items: [globalRole, itemRole] });
  });

  it("shows loading state", () => {
    mockListJenkinsRoles.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<JenkinsRoles />);
    expect(screen.getByText("Loading Jenkins roles...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListJenkinsRoles.mockRejectedValue(new Error("API unavailable"));
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load/)).toBeInTheDocument();
    });
    expect(screen.getByText(/API unavailable/)).toBeInTheDocument();
  });

  it("shows empty state", async () => {
    mockListJenkinsRoles.mockResolvedValue({ items: [] });
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("No Jenkins roles found.")).toBeInTheDocument();
    });
  });

  it("renders page heading and description", async () => {
    renderWithProviders(<JenkinsRoles />);
    expect(screen.getByText("Jenkins Roles")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/JenkinsRole/)).toBeInTheDocument();
    });
  });

  it("renders a semantic table and places permissions under their role type", async () => {
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
      expect(screen.getByText("jenkins-item-builder")).toBeInTheDocument();
    });
    // Role types — also appear in filter chips, use getAllByText
    expect(screen.getAllByText("Global").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Item").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByRole("table")).toBeInTheDocument();
    const globalRow = screen.getByText("jenkins-global-reader").closest("tr")!;
    expect(within(globalRow).getByText("hudson.model.Hudson.Read")).toHaveAttribute("data-label", "Global");
    const itemRow = screen.getByText("jenkins-item-builder").closest("tr")!;
    expect(within(itemRow).getByText("hudson.model.Item.Build")).toHaveAttribute("data-label", "Item");
    expect(within(itemRow).getByText("core")).toHaveAttribute("data-label", "Cluster");
  });

  it("navigates to create page on New Jenkins Role click", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });
    await user.click(screen.getByText("＋ New Jenkins Role"));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-roles/create?cluster=core");
  });

  it("navigates to edit page per role", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });
    await user.click(screen.getAllByText("Edit")[0]);
    expect(mockNavigate).toHaveBeenCalledWith(expect.stringContaining("/edit"));
  });

  it("shows delete confirmation and executes deletion", async () => {
    mockDeleteJenkinsRole.mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });

    await user.click(screen.getAllByText("Delete")[0]);
    expect(screen.getByText("Delete Jenkins Role")).toBeInTheDocument();

    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    const confirmBtn = within(cancelBtn.parentElement!).getByText("Delete");
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(mockDeleteJenkinsRole).toHaveBeenCalledWith("core", "jenkins-global-reader");
    });
  });

  it("filters roles by type (Global / Item / Agent)", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
      expect(screen.getByText("jenkins-item-builder")).toBeInTheDocument();
    });

    // Click Item filter chip (button text is "Item N" with count, use regex)
    await user.click(screen.getByRole("button", { name: /^Item/ }));
    expect(screen.queryByText("jenkins-global-reader")).not.toBeInTheDocument();
    expect(screen.getByText("jenkins-item-builder")).toBeInTheDocument();
  });

  it("shows empty filter result message when filter matches nothing", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: /^Agent/ }));
    expect(screen.getByText(/No Jenkins roles match the current filters/)).toBeInTheDocument();
  });
});
