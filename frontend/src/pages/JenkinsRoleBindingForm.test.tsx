import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoleBindingForm from "./JenkinsRoleBindingForm";
import { ApiError } from "../hooks/useApi";
import { renderWithProviders } from "../test/render-utils";
import { createJenkinsRole, createJenkinsRoleBinding } from "../test/factories";

const mockNavigate = vi.fn();
const mockParams: { name?: string } = {};
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => mockParams };
});

const mockListJenkinsRoles = vi.fn();
const mockGetJenkinsRoleBinding = vi.fn();
const mockCreateJenkinsRoleBinding = vi.fn();
const mockUpdateJenkinsRoleBinding = vi.fn();
const CORE_CLUSTER = { name: "core", core: true, healthy: true, state: "active" };
const MEMBER_CLUSTER = { name: "member-b", core: false, healthy: true, state: "active" };
const mockClusterList = { items: [CORE_CLUSTER] };

vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve(mockClusterList.items),
  listJenkinsRoles: (...args: unknown[]) => mockListJenkinsRoles(...args),
  getJenkinsRoleBinding: (...args: unknown[]) => mockGetJenkinsRoleBinding(...args),
  createJenkinsRoleBinding: (...args: unknown[]) => mockCreateJenkinsRoleBinding(...args),
  updateJenkinsRoleBinding: (...args: unknown[]) => mockUpdateJenkinsRoleBinding(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: {} }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const availableJenkinsRoles = [
  createJenkinsRole({ metadata: { name: "jenkins-viewer" }, spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Read"] } }),
  createJenkinsRole({ metadata: { name: "jenkins-admin" }, spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Administer"] } }),
];

const existingBinding = createJenkinsRoleBinding({
  metadata: { name: "my-jenkins-binding" },
  spec: {
    subjects: [{ kind: "User", name: "alice" }],
    roleRef: "jenkins-admin",
    controllerScope: { namespaces: ["infra"], controllerSelector: { matchLabels: { tier: "prod" } } },
    jenkinsScope: { type: "Folder", folder: "tenants/infra", propagate: "Children" },
  },
});

// Helper: labels are siblings to their input elements (no htmlFor).
function getNameInput(): HTMLInputElement {
  return screen.getByText("Name").nextElementSibling as HTMLInputElement;
}

describe("JenkinsRoleBindingForm — create mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = undefined;
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
  });

  it("renders create heading and all form sections", async () => {
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });
    expect(getNameInput()).toBeInTheDocument();
    expect(screen.getByText("Subjects")).toBeInTheDocument();
    expect(screen.getByText("Role Ref")).toBeInTheDocument();
    expect(screen.getByText(/Controller Scope/)).toBeInTheDocument();
    expect(screen.getByText(/Jenkins Scope/)).toBeInTheDocument();
  });

  it("has name field editable and required", async () => {
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });
    const nameInput = getNameInput();
    expect(nameInput).not.toBeDisabled();
    expect(nameInput).toHaveAttribute("required");
  });

  it("submits and navigates to /jenkins-rolebindings on create", async () => {
    mockCreateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });

    await user.type(getNameInput(), "new-jenkins-binding");
    // Fill in subject
    const subjectInputs = screen.getAllByPlaceholderText("Subject name");
    await user.type(subjectInputs[0], "platform-team");
    // Select role (combobox[0] is subject kind select, combobox[1] is role ref select)
    const combos = screen.getAllByRole("combobox");
    await user.selectOptions(combos[1], "jenkins-viewer");

    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateJenkinsRoleBinding).toHaveBeenCalledWith("core", 
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "new-jenkins-binding" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-role-bindings?cluster=core");
    });
  });

  it("shows error on create failure", async () => {
    mockCreateJenkinsRoleBinding.mockRejectedValueOnce(new Error("Bind failed"));
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });

    await user.type(getNameInput(), "fail");
    // Fill required fields so form submits
    const subjectInputs = screen.getAllByPlaceholderText("Subject name");
    await user.type(subjectInputs[0], "test-user");
    const combos = screen.getAllByRole("combobox");
    await user.selectOptions(combos[1], "jenkins-viewer");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText("Bind failed")).toBeInTheDocument();
    });
  });

  it("shows the conflict message on a create over an existing name", async () => {
    mockCreateJenkinsRoleBinding.mockRejectedValueOnce(new ApiError(409, "x", { error: "already exists" }));
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });

    await user.type(getNameInput(), "fail");
    // Fill required fields so form submits
    const subjectInputs = screen.getAllByPlaceholderText("Subject name");
    await user.type(subjectInputs[0], "test-user");
    const combos = screen.getAllByRole("combobox");
    await user.selectOptions(combos[1], "jenkins-viewer");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText("A Jenkins role binding with this name already exists.")).toBeInTheDocument();
    });
  });

  it("allows adding and removing subjects", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });

    expect(screen.getAllByPlaceholderText("Subject name").length).toBe(1);
    await user.click(screen.getByText("+ Add Subject"));
    expect(screen.getAllByPlaceholderText("Subject name").length).toBe(2);

    const removeButtons = screen.getAllByText("×");
    await user.click(removeButtons[0]);
    expect(screen.getAllByPlaceholderText("Subject name").length).toBeGreaterThanOrEqual(1);
  });

  it("allows expanding Controller Scope and Jenkins Scope sections", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });

    // Both scopes start collapsed
    expect(screen.queryByPlaceholderText("namespace-a, namespace-b")).not.toBeInTheDocument();
    expect(screen.queryByPlaceholderText("e.g. payments/backend")).not.toBeInTheDocument();

    // Expand Controller Scope
    await user.click(screen.getByText(/Controller Scope/));
    expect(screen.getByPlaceholderText("namespace-a, namespace-b")).toBeInTheDocument();

    // Expand Jenkins Scope
    await user.click(screen.getByText(/Jenkins Scope/));
    // The type selector should be visible
    const combos = screen.getAllByRole("combobox");
    expect(combos.length).toBeGreaterThanOrEqual(3);

    // Select Folder type and see folder input
    await user.selectOptions(combos[2], "Folder");
    expect(screen.getByPlaceholderText("e.g. payments/backend")).toBeInTheDocument();
  });

  it("navigates to /jenkins-rolebindings on Cancel", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-role-bindings?cluster=core");
  });
});

describe("JenkinsRoleBindingForm — edit mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-jenkins-binding";
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
    mockGetJenkinsRoleBinding.mockResolvedValue(existingBinding);
  });

  it("renders edit heading and pre-fills fields", async () => {
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });

    const nameInput = getNameInput();
    expect(nameInput).toBeDisabled();
    expect(nameInput).toHaveValue("my-jenkins-binding");

    expect(screen.getByDisplayValue("alice")).toBeInTheDocument();
  });

  it("pre-fills controller scope and jenkins scope in edit mode", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });

    // Expand Controller Scope
    await user.click(screen.getByText(/Controller Scope/));
    expect(screen.getByDisplayValue("infra")).toBeInTheDocument();

    // Expand Jenkins Scope
    await user.click(screen.getByText(/Jenkins Scope/));
    expect(screen.getByDisplayValue("Folder")).toBeInTheDocument();
    expect(screen.getByDisplayValue("tenants/infra")).toBeInTheDocument();
  });

  it("submits update and navigates to /jenkins-rolebindings", async () => {
    mockUpdateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalledWith("core", 
        "my-jenkins-binding",
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "my-jenkins-binding" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-role-bindings?cluster=core");
    });
  });
});

describe("JenkinsRoleBindingForm — the version of the read", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-jenkins-binding";
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
  });

  it("sends the resourceVersion and labels of the read on an update", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValue(createJenkinsRoleBinding({
      metadata: { name: "my-jenkins-binding", resourceVersion: "7", labels: { owner: "ops" } },
      spec: { subjects: [{ kind: "User", name: "alice" }], roleRef: "jenkins-admin" },
    }));
    mockUpdateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalledTimes(1);
    });
    expect(mockUpdateJenkinsRoleBinding.mock.calls[0][2].metadata).toEqual(
      expect.objectContaining({ name: "my-jenkins-binding", resourceVersion: "7", labels: { owner: "ops" } }),
    );
  });

  it("shows the conflict note when another writer changed the binding", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValue(existingBinding);
    mockUpdateJenkinsRoleBinding.mockRejectedValueOnce(new ApiError(409, "conflict"));
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/Another user or tool changed this Jenkins role binding after you opened it/)).toBeInTheDocument();
    expect(mockNavigate).not.toHaveBeenCalled();
  });
});

describe("JenkinsRoleBindingForm — controller selector match expressions", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-jenkins-binding";
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
    mockUpdateJenkinsRoleBinding.mockResolvedValue({});
  });

  async function openEditWith(scope: Record<string, unknown>) {
    mockGetJenkinsRoleBinding.mockResolvedValueOnce(
      createJenkinsRoleBinding({
        metadata: { name: "my-jenkins-binding" },
        spec: { subjects: [{ kind: "User", name: "alice" }], roleRef: "jenkins-admin", controllerScope: scope },
      }),
    );
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
  }

  it("keeps stored match expressions beside the edited labels on a save", async () => {
    const scope = {
      namespaces: ["infra"],
      controllerSelector: {
        matchLabels: { tier: "prod" },
        matchExpressions: [{ key: "env", operator: "NotIn", values: ["dev"] }],
      },
    };
    await openEditWith(scope);
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalled());
    const sent = mockUpdateJenkinsRoleBinding.mock.calls[0][2];
    expect(sent.spec.controllerScope).toEqual(scope);
  });

  it("keeps an expressions-only selector and does not widen the binding", async () => {
    await openEditWith({ controllerSelector: { matchExpressions: [{ key: "tier", operator: "Exists" }] } });
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalled());
    const sent = mockUpdateJenkinsRoleBinding.mock.calls[0][2];
    expect(sent.spec.controllerScope).toEqual({
      controllerSelector: { matchExpressions: [{ key: "tier", operator: "Exists" }] },
    });
  });

  it("keeps match expressions when the user removes every selector label", async () => {
    await openEditWith({
      controllerSelector: {
        matchLabels: { tier: "prod" },
        matchExpressions: [{ key: "env", operator: "In", values: ["qa"] }],
      },
    });
    const user = userEvent.setup();
    await user.click(screen.getByText(/Controller Scope/));
    const labelRow = screen.getByDisplayValue("prod").closest("div")!;
    await user.click(within(labelRow).getByRole("button", { name: "×" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalled());
    const sent = mockUpdateJenkinsRoleBinding.mock.calls[0][2];
    expect(sent.spec.controllerScope.controllerSelector).toEqual({
      matchExpressions: [{ key: "env", operator: "In", values: ["qa"] }],
    });
    expect(sent.spec.controllerScope.controllerSelector).not.toHaveProperty("matchLabels");
  });

  it("lists stored match expressions read-only with a note", async () => {
    await openEditWith({
      controllerSelector: {
        matchExpressions: [
          { key: "tier", operator: "In", values: ["gold", "silver"] },
          { key: "env", operator: "Exists" },
        ],
      },
    });
    const user = userEvent.setup();
    await user.click(screen.getByText(/Controller Scope/));

    expect(screen.getByText("Controller Selector Expressions")).toBeInTheDocument();
    expect(screen.getByText("tier In (gold, silver)")).toBeInTheDocument();
    expect(screen.getByText("env Exists")).toBeInTheDocument();
    expect(screen.getByText(/The dashboard does not edit match expressions/)).toBeInTheDocument();
    expect(screen.getByText(/The stored selector expressions still narrow this binding/)).toBeInTheDocument();
    expect(screen.queryByDisplayValue(/gold/)).toBeNull();
  });

  it("shows no expression list for a binding without match expressions", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValueOnce(existingBinding);
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    const user = userEvent.setup();
    await user.click(screen.getByText(/Controller Scope/));
    expect(screen.queryByText("Controller Selector Expressions")).toBeNull();
  });

  it("sends no controller scope when the user clears a scope that has no expressions", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValueOnce(existingBinding);
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    const user = userEvent.setup();
    await user.click(screen.getByText(/Controller Scope/));
    await user.clear(screen.getByPlaceholderText("namespace-a, namespace-b"));
    const labelRow = screen.getByDisplayValue("prod").closest("div")!;
    await user.click(within(labelRow).getByRole("button", { name: "×" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalled());
    const sent = mockUpdateJenkinsRoleBinding.mock.calls[0][2];
    expect(sent.spec.controllerScope).toBeUndefined();
  });
});

describe("JenkinsRoleBindingForm — a federated copy", () => {
  const copy = createJenkinsRoleBinding({
    metadata: {
      name: "my-jenkins-binding",
      labels: { "varroa.dev/federated-from": "writer" },
      annotations: { "varroa.dev/federated-bindings": "bind-a,bind-b" },
    },
    spec: {
      subjects: [{ kind: "Group", name: "ops" }],
      roleRef: "jenkins-admin",
      jenkinsScope: { type: "Folder", folder: "tenants/infra", propagate: "Children" },
    },
  });

  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-jenkins-binding";
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
    mockGetJenkinsRoleBinding.mockResolvedValue(copy);
  });

  it("renders a federated copy read-only with a note that names the source", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    expect(screen.getByRole("note")).toHaveTextContent(
      "This Jenkins role binding is a federated copy of VarroaRoleBinding bind-a, bind-b.",
    );
    expect(getNameInput()).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Jenkins Scope/ })).toBeDisabled();
    expect(screen.getByDisplayValue("tenants/infra")).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-role-bindings?cluster=core");
  });

  it("shows a tenant-member roleRef that is not a listed role", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValue(createJenkinsRoleBinding({
      metadata: { name: "my-jenkins-binding" },
      spec: { subjects: [{ kind: "Group", name: "ops" }], roleRef: "varroa:tenant-member:alpha" },
    }));
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    const select = screen.getByText("Role Ref").nextElementSibling as HTMLSelectElement;
    expect(select).toHaveValue("varroa:tenant-member:alpha");
  });

  it("shows the server message on a federated_copy refusal", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValue(createJenkinsRoleBinding({
      metadata: { name: "my-jenkins-binding", resourceVersion: "7" },
      spec: { subjects: [{ kind: "Group", name: "ops" }], roleRef: "jenkins-admin" },
    }));
    mockUpdateJenkinsRoleBinding.mockRejectedValueOnce(
      new ApiError(409, "conflict", { error: "JenkinsRoleBinding my-jenkins-binding is a federated copy.", code: "federated_copy" }),
    );
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("JenkinsRoleBinding my-jenkins-binding is a federated copy.")).toBeInTheDocument();
    expect(mockNavigate).not.toHaveBeenCalled();
  });
});

describe("JenkinsRoleBindingForm — an rbacSpec binding", () => {
  const owned = createJenkinsRoleBinding({
    metadata: {
      name: "ctrl-fbff6b43b023e716-admin",
      labels: {
        "varroa.dev/managed-by": "varroa-operator",
        "varroa.dev/controller-namespace": "prod-a",
        "varroa.dev/controller-name": "jenkins-1",
      },
    },
    spec: {
      subjects: [{ kind: "Group", name: "ops" }],
      roleRef: "jenkins-admin",
      jenkinsScope: { type: "Global" },
    },
  });

  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "ctrl-fbff6b43b023e716-admin";
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
    mockGetJenkinsRoleBinding.mockResolvedValue(owned);
  });

  it("renders an rbacSpec binding read-only with a note that links to the controller", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Jenkins Role Binding: ctrl-fbff6b43b023e716-admin")).toBeInTheDocument();
    });
    const note = screen.getByRole("note");
    expect(note).toHaveTextContent(
      "The operator writes this Jenkins role binding from the rbacSpec of controller prod-a/jenkins-1.",
    );
    expect(within(note).getByRole("link", { name: "prod-a/jenkins-1" })).toHaveAttribute(
      "href",
      "/controllers/core/prod-a/jenkins-1",
    );
    expect(getNameInput()).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-role-bindings?cluster=core");
  });

  it("keeps a renamed copy that carries the labels editable", async () => {
    mockGetJenkinsRoleBinding.mockResolvedValue(createJenkinsRoleBinding({
      metadata: { name: "payments-admins", labels: owned.metadata.labels },
      spec: { subjects: [{ kind: "Group", name: "ops" }], roleRef: "jenkins-admin" },
    }));
    mockParams.name = "payments-admins";
    renderWithProviders(<JenkinsRoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: payments-admins")).toBeInTheDocument();
    });
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });
});

describe("JenkinsRoleBindingForm — cluster switch", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = undefined;
    mockClusterList.items = [CORE_CLUSTER, MEMBER_CLUSTER];
  });

  afterEach(() => {
    mockClusterList.items = [CORE_CLUSTER];
  });

  it("keeps the role options of the selected cluster when the read of the previous cluster ends later", async () => {
    const user = userEvent.setup();
    let resolveMember!: (value: { items: ReturnType<typeof createJenkinsRole>[] }) => void;
    mockListJenkinsRoles.mockImplementation((cluster: string) =>
      cluster === "member-b"
        ? new Promise<{ items: ReturnType<typeof createJenkinsRole>[] }>((resolve) => {
            resolveMember = resolve;
          })
        : Promise.resolve({ items: [createJenkinsRole({ metadata: { name: "core-viewer" } })] }),
    );
    renderWithProviders(<JenkinsRoleBindingForm />);

    const clusterSelect = (await screen.findByRole("option", { name: "member-b" })).closest("select")!;
    await screen.findByRole("option", { name: "core-viewer" });

    await user.selectOptions(clusterSelect, "member-b");
    await user.selectOptions(clusterSelect, "core");

    await act(async () => {
      resolveMember({ items: [createJenkinsRole({ metadata: { name: "member-viewer" } })] });
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByRole("option", { name: "core-viewer" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "member-viewer" })).not.toBeInTheDocument();
  });
});

describe("JenkinsRoleBindingForm — Jenkins scope fields", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = undefined;
    mockListJenkinsRoles.mockResolvedValue({ items: availableJenkinsRoles });
  });

  // fillCreateForm fills the name, the subject, and the role, then opens the
  // Jenkins Scope section. Comboboxes: [0] subject kind, [1] role ref,
  // [2] scope type, [3] propagate (Folder only).
  async function fillCreateForm(user: ReturnType<typeof userEvent.setup>) {
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Create Jenkins Role Binding")).toBeInTheDocument();
    });
    await user.type(getNameInput(), "scoped-binding");
    await user.type(screen.getAllByPlaceholderText("Subject name")[0], "test-user");
    await user.selectOptions(screen.getAllByRole("combobox")[1], "jenkins-viewer");
    await user.click(screen.getByText(/Jenkins Scope/));
  }

  function savedJenkinsScope(): unknown {
    const body = mockCreateJenkinsRoleBinding.mock.calls[0][1] as {
      spec: { jenkinsScope?: unknown };
    };
    return body.spec.jenkinsScope;
  }

  it("refuses a Folder scope with no folder before a save", async () => {
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Folder");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("A Folder scope needs a folder name.")).toBeInTheDocument();
    expect(mockCreateJenkinsRoleBinding).not.toHaveBeenCalled();
  });

  it("refuses a folder with a leading slash before a save", async () => {
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Folder");
    await user.type(screen.getByPlaceholderText("e.g. payments/backend"), "/payments");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(
      await screen.findByText("Enter the folder name with no leading slash."),
    ).toBeInTheDocument();
    expect(mockCreateJenkinsRoleBinding).not.toHaveBeenCalled();
  });

  it("refuses a Pattern scope with no pattern before a save", async () => {
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Pattern");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("A Pattern scope needs a pattern.")).toBeInTheDocument();
    expect(mockCreateJenkinsRoleBinding).not.toHaveBeenCalled();
  });

  it("shows a Folder placeholder and hint with no leading slash", async () => {
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Folder");
    expect(screen.getByPlaceholderText("e.g. payments/backend")).toBeInTheDocument();
    expect(
      screen.getByText("Enter the full name of the folder, with no leading slash."),
    ).toBeInTheDocument();
  });

  it("shows Propagate only for a Folder scope", async () => {
    const user = userEvent.setup();
    await fillCreateForm(user);
    const typeSelect = screen.getAllByRole("combobox")[2];

    await user.selectOptions(typeSelect, "Global");
    expect(screen.queryByText("Propagate")).not.toBeInTheDocument();

    await user.selectOptions(typeSelect, "Pattern");
    expect(screen.queryByText("Propagate")).not.toBeInTheDocument();

    await user.selectOptions(typeSelect, "Folder");
    expect(screen.getByText("Propagate")).toBeInTheDocument();
  });

  it("sends propagate for a Folder scope", async () => {
    mockCreateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Folder");
    await user.type(screen.getByPlaceholderText("e.g. payments/backend"), "payments/backend");
    await user.selectOptions(screen.getAllByRole("combobox")[3], "Subtree");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateJenkinsRoleBinding).toHaveBeenCalled();
    });
    expect(savedJenkinsScope()).toEqual({
      type: "Folder",
      folder: "payments/backend",
      propagate: "Subtree",
    });
  });

  it("drops a chosen propagate when the type changes from Folder to Global", async () => {
    mockCreateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Folder");
    await user.selectOptions(screen.getAllByRole("combobox")[3], "Subtree");
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Global");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateJenkinsRoleBinding).toHaveBeenCalled();
    });
    expect(savedJenkinsScope()).toEqual({ type: "Global" });
  });

  it("drops a chosen propagate when the type changes to Pattern", async () => {
    mockCreateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    await fillCreateForm(user);
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Folder");
    await user.selectOptions(screen.getAllByRole("combobox")[3], "Subtree");
    await user.selectOptions(screen.getAllByRole("combobox")[2], "Pattern");
    await user.type(screen.getByPlaceholderText("e.g. .*tenant-a.*"), "payments/.*");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateJenkinsRoleBinding).toHaveBeenCalled();
    });
    expect(savedJenkinsScope()).toEqual({ type: "Pattern", pattern: "payments/.*" });
  });

  it("drops a stored propagate from a Global scope on save", async () => {
    mockParams.name = "my-jenkins-binding";
    mockGetJenkinsRoleBinding.mockResolvedValue(
      createJenkinsRoleBinding({
        metadata: { name: "my-jenkins-binding" },
        spec: {
          subjects: [{ kind: "User", name: "alice" }],
          roleRef: "jenkins-admin",
          jenkinsScope: { type: "Global", propagate: "Subtree" },
        },
      }),
    );
    mockUpdateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalled();
    });
    const body = mockUpdateJenkinsRoleBinding.mock.calls[0][2] as { spec: { jenkinsScope?: unknown } };
    expect(body.spec.jenkinsScope).toEqual({ type: "Global" });
  });

  it("keeps a stored propagate on a Folder scope", async () => {
    mockParams.name = "my-jenkins-binding";
    mockGetJenkinsRoleBinding.mockResolvedValue(existingBinding);
    mockUpdateJenkinsRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role Binding: my-jenkins-binding")).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateJenkinsRoleBinding).toHaveBeenCalled();
    });
    const body = mockUpdateJenkinsRoleBinding.mock.calls[0][2] as { spec: { jenkinsScope?: unknown } };
    expect(body.spec.jenkinsScope).toEqual({
      type: "Folder",
      folder: "tenants/infra",
      propagate: "Children",
    });
  });
});
