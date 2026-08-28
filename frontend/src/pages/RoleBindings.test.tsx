import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import RoleBindings from "./RoleBindings";
import { renderWithProviders } from "../test/render-utils";
import { createRoleBinding } from "../test/factories";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListRoleBindings = vi.fn();
const mockDeleteRoleBinding = vi.fn();
vi.mock("../api/client", () => ({
  listRoleBindings: (...args: unknown[]) => mockListRoleBindings(...args),
  deleteRoleBinding: (...args: unknown[]) => mockDeleteRoleBinding(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: () => true,
}));

const clusterWideBinding = createRoleBinding({
  metadata: { name: "cluster-binding" },
  spec: { subjects: [{ kind: "User", name: "alice" }], roleRef: "varroa:admin" },
});

const scopedBinding = createRoleBinding({
  metadata: { name: "ns-binding" },
  spec: {
    subjects: [{ kind: "Group", name: "developers" }, { kind: "User", name: "bob" }],
    roleRef: "varroa:operator",
    scope: { namespaces: ["team-a", "team-b"] },
  },
});

describe("RoleBindings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListRoleBindings.mockResolvedValue({ items: [clusterWideBinding, scopedBinding] });
  });

  it("shows loading state", () => {
    mockListRoleBindings.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<RoleBindings />);
    expect(screen.getByText("Loading role bindings...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListRoleBindings.mockRejectedValue(new Error("Server error"));
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Server error/)).toBeInTheDocument();
  });

  it("shows empty state", async () => {
    mockListRoleBindings.mockResolvedValue({ items: [] });
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("No role bindings found.")).toBeInTheDocument();
    });
  });

  it("renders page heading and description", async () => {
    renderWithProviders(<RoleBindings />);
    expect(screen.getByText("Role Bindings")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/VarroaRoleBinding/)).toBeInTheDocument();
    });
  });

  it("renders table with name, subjects, role ref, and scope", async () => {
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("cluster-binding")).toBeInTheDocument();
      expect(screen.getByText("ns-binding")).toBeInTheDocument();
    });
    // Subjects
    expect(screen.getByText("User:alice")).toBeInTheDocument();
    expect(screen.getByText("Group:developers, User:bob")).toBeInTheDocument();
    // Role refs
    expect(screen.getByText("varroa:admin")).toBeInTheDocument();
    expect(screen.getByText("varroa:operator")).toBeInTheDocument();
    // Scope — the filter chip also contains "Cluster-wide" text, so use getAllByText
    expect(screen.getAllByText("Cluster-wide").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("ns:team-a,team-b")).toBeInTheDocument();
  });

  it('navigates to /rolebindings/create on "＋ New Role Binding" click', async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("cluster-binding")).toBeInTheDocument();
    });
    await user.click(screen.getByText("＋ New Role Binding"));
    expect(mockNavigate).toHaveBeenCalledWith("/access/role-bindings/create");
  });

  it("navigates to edit page for each binding", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("cluster-binding")).toBeInTheDocument();
    });
    const editButtons = screen.getAllByText("Edit");
    await user.click(editButtons[0]);
    expect(mockNavigate).toHaveBeenCalledWith(expect.stringContaining("/edit"));
  });

  it("shows delete confirmation and executes deletion", async () => {
    mockDeleteRoleBinding.mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("cluster-binding")).toBeInTheDocument();
    });

    await user.click(screen.getAllByText("Delete")[0]);

    expect(screen.getByText("Delete Role Binding")).toBeInTheDocument();
    expect(screen.getByText(/Are you sure you want to delete/)).toBeInTheDocument();

    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    const confirmBtn = within(cancelBtn.parentElement!).getByText("Delete");
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(mockDeleteRoleBinding).toHaveBeenCalledWith("cluster-binding");
    });
  });

  it("filters bindings by scope (Cluster-wide / Scoped)", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("cluster-binding")).toBeInTheDocument();
      expect(screen.getByText("ns-binding")).toBeInTheDocument();
    });

    // Filter by "Scoped"
    await user.click(screen.getByText("Scoped"));
    expect(screen.queryByText("cluster-binding")).not.toBeInTheDocument();
    expect(screen.getByText("ns-binding")).toBeInTheDocument();
  });
});
