import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoleBindings from "./JenkinsRoleBindings";
import { renderWithProviders } from "../test/render-utils";
import { createJenkinsRoleBinding } from "../test/factories";
import type { JenkinsRoleBinding } from "../types";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListJenkinsRoleBindings = vi.fn();
const mockDeleteJenkinsRoleBinding = vi.fn();
const CORE_CLUSTER = { name: "core", core: true, healthy: true, state: "active" };
const MEMBER_CLUSTER = { name: "member-b", core: false, healthy: true, state: "active" };
const mockClusterList = { items: [CORE_CLUSTER] };

vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve(mockClusterList.items),
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
    jenkinsScope: { type: "Folder", folder: "tenants/backend" },
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
    expect(screen.getByText("Folder: tenants/backend")).toBeInTheDocument();
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

  it("shows View and a Federated badge for a federated copy", async () => {
    const user = userEvent.setup();
    const copyBinding = createJenkinsRoleBinding({
      metadata: {
        name: "varroa-fed-writer-0123456789ab",
        labels: { "varroa.dev/federated-from": "writer" },
        annotations: { "varroa.dev/federated-bindings": "bind-a" },
      },
      spec: {
        subjects: [{ kind: "Group", name: "writers" }],
        roleRef: "writer",
        jenkinsScope: { type: "Global" },
      },
    });
    mockListJenkinsRoleBindings.mockResolvedValue({ items: [folderScopeBinding, copyBinding] });
    renderWithProviders(<JenkinsRoleBindings />);
    await screen.findByText("varroa-fed-writer-0123456789ab");

    const copyName = screen.getByText("varroa-fed-writer-0123456789ab");
    expect(copyName.closest("td")).toHaveAttribute("data-label", "Name");
    const copyRow = copyName.closest("tr")!;
    expect(within(copyRow).getByText("Federated")).toHaveAttribute(
      "title",
      "Federated copy of VarroaRoleBinding bind-a. Change the source on the core cluster.",
    );
    expect(within(copyRow).queryByRole("button", { name: "Edit" })).toBeNull();
    expect(within(copyRow).getByRole("button", { name: "Delete" })).toBeInTheDocument();

    const plainRow = screen.getByText("jenkins-binding-folder").closest("tr")!;
    expect(within(plainRow).getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(within(plainRow).queryByText("Federated")).toBeNull();

    await user.click(within(copyRow).getByRole("button", { name: "View" }));
    expect(mockNavigate).toHaveBeenCalledWith(
      "/access/jenkins-role-bindings/varroa-fed-writer-0123456789ab/edit?cluster=core",
    );
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

describe("JenkinsRoleBindings cluster switch", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockClusterList.items = [CORE_CLUSTER, MEMBER_CLUSTER];
  });

  afterEach(() => {
    mockClusterList.items = [CORE_CLUSTER];
  });

  it("keeps the bindings of the selected cluster when the read of the previous cluster ends later", async () => {
    let resolveCore!: (value: { items: JenkinsRoleBinding[] }) => void;
    const memberBinding = createJenkinsRoleBinding({ metadata: { name: "member-b-binding" } });
    mockListJenkinsRoleBindings.mockImplementation((cluster: string) =>
      cluster === "core"
        ? new Promise<{ items: JenkinsRoleBinding[] }>((resolve) => {
            resolveCore = resolve;
          })
        : Promise.resolve({ items: [memberBinding] }),
    );
    renderWithProviders(<JenkinsRoleBindings />);

    await userEvent.selectOptions(await screen.findByRole("combobox"), "member-b");
    await screen.findByText("member-b-binding");

    await act(async () => {
      resolveCore({ items: [folderScopeBinding] });
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByText("member-b-binding")).toBeInTheDocument();
    expect(screen.queryByText("jenkins-binding-folder")).not.toBeInTheDocument();
  });
});

const RBAC_SPEC_LABELS = {
  "varroa.dev/managed-by": "varroa-operator",
  "varroa.dev/controller-namespace": "prod-a",
  "varroa.dev/controller-name": "jenkins-1",
};

const rbacSpecBinding = createJenkinsRoleBinding({
  metadata: { name: "ctrl-fbff6b43b023e716-admin", labels: RBAC_SPEC_LABELS },
  spec: {
    subjects: [{ kind: "Group", name: "ops" }],
    roleRef: "jenkins-admin",
    jenkinsScope: { type: "Global" },
  },
});

describe("JenkinsRoleBindings rbacSpec controller", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListJenkinsRoleBindings.mockResolvedValue({ items: [folderScopeBinding, rbacSpecBinding] });
  });

  it("shows a Controller badge that links to the controller of an rbacSpec binding", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindings />);
    await screen.findByText("ctrl-fbff6b43b023e716-admin");

    const row = screen.getByText("ctrl-fbff6b43b023e716-admin").closest("tr")!;
    const badge = within(row).getByRole("link", { name: "Controller: prod-a/jenkins-1" });
    expect(badge).toHaveAttribute("href", "/controllers/core/prod-a/jenkins-1");
    expect(within(row).queryByRole("button", { name: "Edit" })).toBeNull();
    expect(within(row).getByRole("button", { name: "Delete" })).toBeInTheDocument();

    const plainRow = screen.getByText("jenkins-binding-folder").closest("tr")!;
    expect(within(plainRow).queryByRole("link", { name: /Controller:/ })).toBeNull();
    expect(within(plainRow).getByRole("button", { name: "Edit" })).toBeInTheDocument();

    await user.click(within(row).getByRole("button", { name: "View" }));
    expect(mockNavigate).toHaveBeenCalledWith(
      "/access/jenkins-role-bindings/ctrl-fbff6b43b023e716-admin/edit?cluster=core",
    );
  });

  it("shows no Controller badge for a renamed copy that keeps the labels", async () => {
    const renamed = createJenkinsRoleBinding({
      metadata: { name: "payments-admins", labels: RBAC_SPEC_LABELS },
      spec: { subjects: [{ kind: "Group", name: "ops" }], roleRef: "jenkins-admin" },
    });
    mockListJenkinsRoleBindings.mockResolvedValue({ items: [renamed] });
    renderWithProviders(<JenkinsRoleBindings />);
    await screen.findByText("payments-admins");

    const row = screen.getByText("payments-admins").closest("tr")!;
    expect(within(row).queryByRole("link", { name: /Controller:/ })).toBeNull();
    expect(within(row).getByRole("button", { name: "Edit" })).toBeInTheDocument();
  });

  it("matches the controller in the filter", async () => {
    renderWithProviders(<JenkinsRoleBindings />);
    await screen.findByText("ctrl-fbff6b43b023e716-admin");

    const filter = screen.getByPlaceholderText("Filter by name or controller...");
    await userEvent.type(filter, "jenkins-1");

    expect(screen.getByText("ctrl-fbff6b43b023e716-admin")).toBeInTheDocument();
    expect(screen.queryByText("jenkins-binding-folder")).not.toBeInTheDocument();
  });
});

describe("JenkinsRoleBindings rbacSpec controller on a member cluster", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockClusterList.items = [CORE_CLUSTER, MEMBER_CLUSTER];
    mockListJenkinsRoleBindings.mockImplementation((cluster: string) =>
      Promise.resolve({ items: cluster === "member-b" ? [rbacSpecBinding] : [] }),
    );
  });

  afterEach(() => {
    mockClusterList.items = [CORE_CLUSTER];
  });

  it("links the Controller badge to the selected member cluster", async () => {
    renderWithProviders(<JenkinsRoleBindings />);

    await userEvent.selectOptions(await screen.findByRole("combobox"), "member-b");
    const badge = await screen.findByRole("link", { name: "Controller: prod-a/jenkins-1" });
    expect(badge).toHaveAttribute("href", "/controllers/member-b/prod-a/jenkins-1");
  });
});

describe("JenkinsRoleBindings no accessible cluster", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListJenkinsRoleBindings.mockResolvedValue({ items: [folderScopeBinding] });
    mockClusterList.items = [];
  });

  afterEach(() => {
    mockClusterList.items = [CORE_CLUSTER];
  });

  it("ends the loading state and shows no rows when no cluster is accessible", async () => {
    renderWithProviders(<JenkinsRoleBindings />);
    await waitFor(() => {
      expect(screen.queryByText("Loading Jenkins role bindings...")).not.toBeInTheDocument();
    });
    expect(screen.queryByText("jenkins-binding-folder")).not.toBeInTheDocument();
  });

  it("clears existing bindings when no cluster remains", async () => {
    mockClusterList.items = [CORE_CLUSTER];
    const { queryClient } = renderWithProviders(<JenkinsRoleBindings />);
    await screen.findByText("jenkins-binding-folder");

    mockClusterList.items = [];
    void queryClient.invalidateQueries({ queryKey: ["clusters"] });

    await waitFor(() => expect(screen.queryByText("jenkins-binding-folder")).not.toBeInTheDocument());
  });
});
