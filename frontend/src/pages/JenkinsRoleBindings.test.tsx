import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoleBindings from "./JenkinsRoleBindings";
import { renderWithProviders } from "../test/render-utils";
import { createJenkinsRoleBinding } from "../test/factories";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListJenkinsRoleBindings = vi.fn();
const mockDeleteJenkinsRoleBinding = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  listJenkinsRoleBindings: (...args: unknown[]) => mockListJenkinsRoleBindings(...args),
  deleteJenkinsRoleBinding: (...args: unknown[]) => mockDeleteJenkinsRoleBinding(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: () => true,
}));

const folderScopeBinding = createJenkinsRoleBinding({
  metadata: { name: "jenkins-binding-folder" },
  spec: {
    subjects: [{ kind: "Group", name: "developers" }],
    roleRef: "jenkins-global-reader",
    jenkinsScope: { type: "Folder", folder: "/teams/backend" },
  },
});

const globalScopeBinding = createJenkinsRoleBinding({
  metadata: { name: "jenkins-binding-global" },
  spec: {
    subjects: [{ kind: "User", name: "alice" }],
    roleRef: "jenkins-admin",
    jenkinsScope: { type: "Global" },
  },
});

describe("JenkinsRoleBindings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListJenkinsRoleBindings.mockResolvedValue({ items: [folderScopeBinding, globalScopeBinding] });
  });

  it("shows loading state", () => {
    mockListJenkinsRoleBindings.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<JenkinsRoleBindings />);
    expect(screen.getByText("Loading Jenkins role bindings...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListJenkinsRoleBindings.mockRejectedValue(new Error("Connection refused"));
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Connection refused/)).toBeInTheDocument();
  });

  it("shows empty state", async () => {
    mockListJenkinsRoleBindings.mockResolvedValue({ items: [] });
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("No Jenkins role bindings found.")).toBeInTheDocument();
    });
  });

  it("renders page heading and description", async () => {
    renderWithProviders(<JenkinsRoleBindings />);
    expect(screen.getByText("Jenkins Role Bindings")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/JenkinsRoleBinding/)).toBeInTheDocument();
    });
  });

  it("renders table with name, role ref, subjects, and jenkins scope", async () => {
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-binding-folder")).toBeInTheDocument();
      expect(screen.getByText("jenkins-binding-global")).toBeInTheDocument();
    });
    // Role refs
    expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    expect(screen.getByText("jenkins-admin")).toBeInTheDocument();
    // Subjects
    expect(screen.getByText("Group: developers")).toBeInTheDocument();
    expect(screen.getByText("User: alice")).toBeInTheDocument();
    // Jenkins scope
    expect(screen.getByText("Folder: /teams/backend")).toBeInTheDocument();
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.getByText("jenkins-binding-folder").closest("td")).toHaveAttribute("data-label", "Name");
  });

  it("navigates to create page", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-binding-folder")).toBeInTheDocument();
    });
    await user.click(screen.getByText("＋ New Jenkins Binding"));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-role-bindings/create?cluster=core");
  });

  it("navigates to edit page per binding", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-binding-folder")).toBeInTheDocument();
    });
    await user.click(screen.getAllByText("Edit")[0]);
    expect(mockNavigate).toHaveBeenCalledWith(expect.stringContaining("/edit"));
  });

  it("shows delete confirmation and executes deletion", async () => {
    mockDeleteJenkinsRoleBinding.mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-binding-folder")).toBeInTheDocument();
    });

    await user.click(screen.getAllByText("Delete")[0]);
    expect(screen.getByText("Delete Jenkins Role Binding")).toBeInTheDocument();

    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    const confirmBtn = within(cancelBtn.parentElement!).getByText("Delete");
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(mockDeleteJenkinsRoleBinding).toHaveBeenCalledWith("core", "jenkins-binding-folder");
    });
  });
});
