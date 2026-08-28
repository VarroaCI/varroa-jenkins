import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Roles from "./Roles";
import { renderWithProviders } from "../test/render-utils";
import { createRole } from "../test/factories";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListRoles = vi.fn();
const mockDeleteRole = vi.fn();
vi.mock("../api/client", () => ({
  listRoles: (...args: unknown[]) => mockListRoles(...args),
  deleteRole: (...args: unknown[]) => mockDeleteRole(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: () => true,
}));

const builtinRole = createRole({
  metadata: { name: "varroa:admin", labels: { "varroa.dev/builtin": "true" } },
  spec: { apiRules: [{ resources: ["*"], verbs: ["*"] }], jenkinsPermissions: ["hudson.model.Hudson.Administer"] },
});

const customRole = createRole({
  metadata: { name: "my-role" },
  spec: { apiRules: [{ resources: ["controllers"], verbs: ["get", "list"] }] },
});

describe("Roles", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListRoles.mockResolvedValue({ items: [builtinRole, customRole] });
  });

  it("shows loading state", () => {
    mockListRoles.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<Roles />);
    expect(screen.getByText("Loading roles...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListRoles.mockRejectedValue(new Error("Failed to fetch"));
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Failed to fetch/)).toBeInTheDocument();
  });

  it("shows empty state", async () => {
    mockListRoles.mockResolvedValue({ items: [] });
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("No roles found.")).toBeInTheDocument();
    });
  });

  it("renders page heading and description", async () => {
    renderWithProviders(<Roles />);
    expect(screen.getByText("Roles")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/VarroaRole/)).toBeInTheDocument();
    });
  });

  it("renders table with role names, built-in status, and permission counts", async () => {
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("varroa:admin")).toBeInTheDocument();
      expect(screen.getByText("my-role")).toBeInTheDocument();
    });
    // Built-in indicator
    expect(screen.getByText("Yes")).toBeInTheDocument();
    expect(screen.getByText("No")).toBeInTheDocument();
    // Permission counts: 1 API rule each, 1 jenkins perm for builtin, 0 for custom
    expect(screen.getAllByText("1").length).toBeGreaterThanOrEqual(2);
  });

  it('navigates to /roles/create when "＋ New Role" is clicked', async () => {
    const user = userEvent.setup();
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("varroa:admin")).toBeInTheDocument();
    });
    await user.click(screen.getByText("＋ New Role"));
    expect(mockNavigate).toHaveBeenCalledWith("/access/roles/create");
  });

  it("navigates to edit page for each role", async () => {
    const user = userEvent.setup();
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("my-role")).toBeInTheDocument();
    });
    const editButtons = screen.getAllByText("Edit");
    await user.click(editButtons[0]);
    expect(mockNavigate).toHaveBeenCalledWith(expect.stringContaining("/edit"));
  });

  it("shows delete confirmation dialog and executes deletion", async () => {
    mockDeleteRole.mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("varroa:admin")).toBeInTheDocument();
    });

    // Click row Delete button
    await user.click(screen.getAllByText("Delete")[0]);

    // Dialog appears
    expect(screen.getByText("Delete Role")).toBeInTheDocument();
    expect(screen.getByText(/Are you sure you want to delete/)).toBeInTheDocument();

    // Find confirm Delete button inside dialog
    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    const dialogActions = cancelBtn.parentElement!;
    const confirmBtn = within(dialogActions).getByText("Delete");

    await user.click(confirmBtn);

    await waitFor(() => {
      expect(mockDeleteRole).toHaveBeenCalledWith("varroa:admin");
      expect(screen.queryByText("Delete Role")).not.toBeInTheDocument();
    });
  });

  it("dismisses delete dialog when Cancel is clicked", async () => {
    const user = userEvent.setup();
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("varroa:admin")).toBeInTheDocument();
    });

    await user.click(screen.getAllByText("Delete")[0]);
    expect(screen.getByText("Delete Role")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("Delete Role")).not.toBeInTheDocument();
  });

  it("filters roles by built-in / custom status", async () => {
    const user = userEvent.setup();
    renderWithProviders(<Roles />);
    await waitFor(() => {
      expect(screen.getByText("varroa:admin")).toBeInTheDocument();
      expect(screen.getByText("my-role")).toBeInTheDocument();
    });

    // Click Built-in filter chip
    await user.click(screen.getByRole("button", { name: /Built-in/ }));
    expect(screen.getByText("varroa:admin")).toBeInTheDocument();
    expect(screen.queryByText("my-role")).not.toBeInTheDocument();
  });
});
