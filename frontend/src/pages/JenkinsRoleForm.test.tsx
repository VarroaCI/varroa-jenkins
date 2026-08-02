import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoleForm from "./JenkinsRoleForm";

vi.mock("../hooks/useConfigurationCluster", () => ({
  useConfigurationCluster: () => ({ cluster: "core", entry: null, ready: true }),
}));
import { renderWithProviders } from "../test/render-utils";
import { createJenkinsRole } from "../test/factories";

const mockNavigate = vi.fn();
const mockParams: { name?: string } = {};
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => mockParams };
});

const mockCreateJenkinsRole = vi.fn();
const mockUpdateJenkinsRole = vi.fn();
const mockGetJenkinsRole = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  createJenkinsRole: (...args: unknown[]) => mockCreateJenkinsRole(...args),
  updateJenkinsRole: (...args: unknown[]) => mockUpdateJenkinsRole(...args),
  getJenkinsRole: (...args: unknown[]) => mockGetJenkinsRole(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: {} }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const existingRole = createJenkinsRole({
  metadata: { name: "jenkins-viewer" },
  spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Read"], description: "Read-only access" },
});

// Helpers: labels are siblings to their input/select elements (no htmlFor).
function getNameInput(): HTMLInputElement {
  return screen.getByText("Name").nextElementSibling as HTMLInputElement;
}
function getRoleTypeSelect(): HTMLSelectElement {
  return screen.getByText("Role Type").nextElementSibling as HTMLSelectElement;
}
function getDescriptionInput(): HTMLInputElement {
  return screen.getByText("Description").nextElementSibling as HTMLInputElement;
}

describe("JenkinsRoleForm — create mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = undefined;
  });

  it("renders create heading and all form fields", () => {
    renderWithProviders(<JenkinsRoleForm />);
    expect(screen.getByText("Create Jenkins Role")).toBeInTheDocument();
    expect(getNameInput()).toBeInTheDocument();
    expect(getRoleTypeSelect()).toBeInTheDocument();
    expect(screen.getByText("Permissions (one per line)")).toBeInTheDocument();
    expect(getDescriptionInput()).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("has name field editable and required", () => {
    renderWithProviders(<JenkinsRoleForm />);
    const nameInput = getNameInput();
    expect(nameInput).not.toBeDisabled();
    expect(nameInput).toHaveAttribute("required");
  });

  it("has role type selector with Global/Item/Agent options", () => {
    renderWithProviders(<JenkinsRoleForm />);
    const typeSelect = getRoleTypeSelect();
    expect(typeSelect).toHaveValue("Global");
    const options = Array.from(typeSelect.querySelectorAll("option")).map((o) => o.textContent);
    expect(options).toEqual(["Global", "Item", "Agent"]);
  });

  it("submits and navigates to /jenkins-roles on create", async () => {
    mockCreateJenkinsRole.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleForm />);

    await user.type(getNameInput(), "new-jenkins-role");
    await user.type(screen.getByText("Permissions (one per line)")
      .closest("div")!
      .querySelector("textarea")!, "hudson.model.Hudson.Read");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateJenkinsRole).toHaveBeenCalledWith("core", 
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "new-jenkins-role" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-roles?cluster=core");
    });
  });

  it("shows error on create failure", async () => {
    mockCreateJenkinsRole.mockRejectedValueOnce(new Error("Invalid role data"));
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleForm />);

    await user.type(getNameInput(), "broken");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText("Invalid role data")).toBeInTheDocument();
    });
  });

  it("navigates to /jenkins-roles on Cancel", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleForm />);
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-roles?cluster=core");
  });
});

describe("JenkinsRoleForm — edit mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "jenkins-viewer";
    mockGetJenkinsRole.mockResolvedValue(existingRole);
  });

  it("renders edit heading and pre-fills fields", async () => {
    renderWithProviders(<JenkinsRoleForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role: jenkins-viewer")).toBeInTheDocument();
    });

    const nameInput = getNameInput();
    expect(nameInput).toBeDisabled();
    expect(nameInput).toHaveValue("jenkins-viewer");

    const typeSelect = getRoleTypeSelect();
    expect(typeSelect).toHaveValue("Global");

    const descInput = getDescriptionInput();
    expect(descInput).toHaveValue("Read-only access");
  });

  it("pre-fills permissions from existing data", async () => {
    renderWithProviders(<JenkinsRoleForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role: jenkins-viewer")).toBeInTheDocument();
    });
    const textarea = screen.getByText("Permissions (one per line)")
      .closest("div")!
      .querySelector("textarea")!;
    expect(textarea).toHaveValue("hudson.model.Hudson.Read");
  });

  it("submits update and navigates to /jenkins-roles", async () => {
    mockUpdateJenkinsRole.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoleForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Jenkins Role: jenkins-viewer")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateJenkinsRole).toHaveBeenCalledWith("core", 
        "jenkins-viewer",
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "jenkins-viewer" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-roles?cluster=core");
    });
  });
});
