import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import RoleBindingForm from "./RoleBindingForm";
import { ApiError } from "../hooks/useApi";
import { renderWithProviders } from "../test/render-utils";
import { createRole, createRoleBinding } from "../test/factories";

const mockNavigate = vi.fn();
const mockParams: { name?: string } = {};
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => mockParams };
});

const mockListRoles = vi.fn();
const mockGetRoleBinding = vi.fn();
const mockCreateRoleBinding = vi.fn();
const mockUpdateRoleBinding = vi.fn();
const mockListTenants = vi.fn().mockResolvedValue([]);
const mockListPipelines = vi.fn().mockResolvedValue({
  items: [],
  runningNow: [],
  tenantFolder: "",
  truncated: false,
});
vi.mock("../api/client", () => ({
  listRoles: (...args: unknown[]) => mockListRoles(...args),
  getRoleBinding: (...args: unknown[]) => mockGetRoleBinding(...args),
  createRoleBinding: (...args: unknown[]) => mockCreateRoleBinding(...args),
  updateRoleBinding: (...args: unknown[]) => mockUpdateRoleBinding(...args),
  listTenants: (...args: unknown[]) => mockListTenants(...args),
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: {} }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const availableRoles = [
  createRole({ metadata: { name: "varroa:admin" } }),
  createRole({ metadata: { name: "varroa:viewer" } }),
];

const existingBinding = createRoleBinding({
  metadata: { name: "my-binding" },
  spec: {
    subjects: [{ kind: "User", name: "alice" }],
    roleRef: "varroa:admin",
    scope: { namespaces: ["team-a"], controllerSelector: { matchLabels: { tenant: "frontend" } } },
  },
});

// Helper: labels are siblings to their input elements (no htmlFor).
function getNameInput(): HTMLInputElement {
  return screen.getByText("Name").nextElementSibling as HTMLInputElement;
}

describe("RoleBindingForm — create mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = undefined;
    mockListRoles.mockResolvedValue({ items: availableRoles });
  });

  it("renders create heading and form fields", async () => {
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });
    expect(getNameInput()).toBeInTheDocument();
    expect(screen.getByText("Subjects")).toBeInTheDocument();
    expect(screen.getByText("Role Ref")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });

  it("has name field editable and required", async () => {
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });
    const nameInput = getNameInput();
    expect(nameInput).not.toBeDisabled();
    expect(nameInput).toHaveAttribute("required");
  });

  it("submits and navigates to /rolebindings on create", async () => {
    mockCreateRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });

    await user.type(getNameInput(), "new-binding");
    // Fill in subject name
    const subjectInputs = screen.getAllByPlaceholderText("Subject name");
    await user.type(subjectInputs[0], "dev-tenant");
    // Select role
    await user.selectOptions(screen.getAllByRole("combobox")[1], "varroa:admin");

    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateRoleBinding).toHaveBeenCalledWith(
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "new-binding" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/role-bindings");
    });
  });

  it("shows error on create failure", async () => {
    mockCreateRoleBinding.mockRejectedValueOnce(new Error("Validation error"));
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });

    await user.type(getNameInput(), "fail-binding");
    // Fill required fields so form submits
    const subjectInputs = screen.getAllByPlaceholderText("Subject name");
    await user.type(subjectInputs[0], "test-user");
    await user.selectOptions(screen.getAllByRole("combobox")[1], "varroa:admin");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText("Validation error")).toBeInTheDocument();
    });
  });

  it("shows the conflict message on a create over an existing name", async () => {
    mockCreateRoleBinding.mockRejectedValueOnce(new ApiError(409, "x", { error: "already exists" }));
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });

    await user.type(getNameInput(), "fail-binding");
    // Fill required fields so form submits
    const subjectInputs = screen.getAllByPlaceholderText("Subject name");
    await user.type(subjectInputs[0], "test-user");
    await user.selectOptions(screen.getAllByRole("combobox")[1], "varroa:admin");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText("A role binding with this name already exists.")).toBeInTheDocument();
    });
  });

  it("navigates to /rolebindings on Cancel", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/role-bindings");
  });

  it("allows adding and removing subjects", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });

    // One subject row by default
    expect(screen.getAllByPlaceholderText("Subject name").length).toBe(1);

    // Add another subject
    await user.click(screen.getByText("+ Add Subject"));
    expect(screen.getAllByPlaceholderText("Subject name").length).toBe(2);

    // Remove the first subject row
    const removeButtons = screen.getAllByText("×");
    await user.click(removeButtons[0]);
    // Still has at least 1 row (component keeps at least 1)
    expect(screen.getAllByPlaceholderText("Subject name").length).toBeGreaterThanOrEqual(1);
  });

  it("allows expanding scope section and adding labels", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Create Role Binding")).toBeInTheDocument();
    });

    // Scope is collapsed by default
    expect(screen.queryByPlaceholderText("namespace-a, namespace-b")).not.toBeInTheDocument();

    // Expand scope
    await user.click(screen.getByText(/Scope/));
    expect(screen.getByPlaceholderText("namespace-a, namespace-b")).toBeInTheDocument();

    // Add a label
    await user.click(screen.getByText("+ Add Label"));
    const labelKeys = screen.getAllByPlaceholderText("Key");
    expect(labelKeys.length).toBe(1);
    await user.type(labelKeys[0], "env");
    const labelValues = screen.getAllByPlaceholderText("Value");
    await user.type(labelValues[0], "prod");
  });
});

describe("RoleBindingForm — edit mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-binding";
    mockListRoles.mockResolvedValue({ items: availableRoles });
    mockGetRoleBinding.mockResolvedValue(existingBinding);
  });

  it("renders edit heading and pre-fills fields", async () => {
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Role Binding: my-binding")).toBeInTheDocument();
    });

    const nameInput = getNameInput();
    expect(nameInput).toBeDisabled();
    expect(nameInput).toHaveValue("my-binding");

    // Subject pre-filled
    expect(screen.getByDisplayValue("alice")).toBeInTheDocument();

    // Role ref pre-filled
    expect(screen.getByDisplayValue("varroa:admin")).toBeInTheDocument();
  });

  it("submits update and navigates to /rolebindings", async () => {
    mockUpdateRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Role Binding: my-binding")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateRoleBinding).toHaveBeenCalledWith(
        "my-binding",
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "my-binding" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/role-bindings");
    });
  });

  it("loads scope data in edit mode", async () => {
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Role Binding: my-binding")).toBeInTheDocument();
    });

    // Expand scope section
    const user = userEvent.setup();
    await user.click(screen.getByText(/Scope/));

    expect(screen.getByDisplayValue("team-a")).toBeInTheDocument();
  });
});

describe("RoleBindingForm — the tenants dimension", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-binding";
    mockListRoles.mockResolvedValue({ items: availableRoles });
    mockListTenants.mockResolvedValue([]);
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [],
      tenantFolder: "",
      truncated: false,
    });
  });

  it("disables the tenants control and states why while a namespace is set", async () => {
    mockGetRoleBinding.mockResolvedValue(existingBinding);
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Role Binding: my-binding")).toBeInTheDocument();
    });

    await user.click(screen.getByText(/Scope/));
    expect(screen.getByRole("button", { name: "+ Add tenant" })).toBeDisabled();
    expect(screen.getByText(/Clear the namespaces and the selector labels/)).toBeInTheDocument();
  });

  it("preserves a stored controllerSelector matchExpressions entry", async () => {
    mockGetRoleBinding.mockResolvedValue(
      createRoleBinding({
        metadata: { name: "my-binding" },
        spec: {
          subjects: [{ kind: "User", name: "alice" }],
          roleRef: "varroa:admin",
          scope: {
            controllerSelector: {
              matchLabels: { tenant: "frontend" },
              matchExpressions: [{ key: "tier", operator: "In", values: ["gold"] }],
            },
          },
        },
      }),
    );
    mockUpdateRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Role Binding: my-binding")).toBeInTheDocument();
    });

    await user.type(screen.getAllByPlaceholderText("Subject name")[0], "-2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateRoleBinding).toHaveBeenCalled();
    });
    const sent = mockUpdateRoleBinding.mock.calls[0][1];
    expect(sent.spec.scope.controllerSelector.matchExpressions).toEqual([
      { key: "tier", operator: "In", values: ["gold"] },
    ]);
  });

  it("preserves an untouched tenant scope across an edit", async () => {
    mockGetRoleBinding.mockResolvedValue(
      createRoleBinding({
        metadata: { name: "my-binding" },
        spec: {
          subjects: [{ kind: "User", name: "alice" }],
          roleRef: "varroa:admin",
          scope: { tenants: [{ name: "payments", jobs: ["release/*"] }] },
        },
      }),
    );
    mockUpdateRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Role Binding: my-binding")).toBeInTheDocument();
    });

    await user.selectOptions(screen.getAllByRole("combobox")[1], "varroa:viewer");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateRoleBinding).toHaveBeenCalled();
    });
    const sent = mockUpdateRoleBinding.mock.calls[0][1];
    expect(sent.spec.scope.tenants).toEqual([{ name: "payments", jobs: ["release/*"] }]);
    expect(sent.spec.roleRef).toBe("varroa:viewer");
  });

  it("shows the read-only hive federation table and never sends status", async () => {
    mockGetRoleBinding.mockResolvedValue({
      ...existingBinding,
      status: {
        observedGeneration: 1,
        federation: [
          { cluster: "member-c", state: "federated", reason: "Federated", message: "JenkinsRoleBinding varroa-fed-x", lastTransitionTime: "2026-09-27T12:00:00Z" },
          { cluster: "edge", state: "failed", reason: "HiveUnavailable", message: "offline", lastTransitionTime: "2026-09-27T12:00:00Z" },
        ],
      },
    });
    mockUpdateRoleBinding.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);

    await waitFor(() => {
      expect(screen.getByText("Hive federation")).toBeInTheDocument();
    });
    expect(screen.getByText("edge")).toBeInTheDocument();
    expect(screen.getByText("HiveUnavailable")).toBeInTheDocument();
    expect(screen.getByText("offline")).toBeInTheDocument();
    expect(screen.getByText("failed")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(mockUpdateRoleBinding).toHaveBeenCalled());
    expect(mockUpdateRoleBinding.mock.calls[0][1].status).toBeUndefined();
  });

  it("states that no hive reports a binding with no status", async () => {
    mockGetRoleBinding.mockResolvedValue(existingBinding);
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText(/No hive reports this binding/)).toBeInTheDocument();
    });
  });
});
