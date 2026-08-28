import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoleBindingForm from "./JenkinsRoleBindingForm";
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
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
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
    jenkinsScope: { type: "Folder", folder: "/teams/infra", propagate: "Children" },
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
    expect(screen.queryByPlaceholderText("e.g. /teams/backend")).not.toBeInTheDocument();

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
    expect(screen.getByPlaceholderText("e.g. /teams/backend")).toBeInTheDocument();
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
    expect(screen.getByDisplayValue("/teams/infra")).toBeInTheDocument();
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
