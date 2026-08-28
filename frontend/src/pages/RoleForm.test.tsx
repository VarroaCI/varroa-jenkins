import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import RoleForm from "./RoleForm";
import { renderWithProviders } from "../test/render-utils";
import { createRole } from "../test/factories";

const mockNavigate = vi.fn();
const mockParams: { name?: string } = {};
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => mockParams };
});

const mockCreateRole = vi.fn();
const mockUpdateRole = vi.fn();
const mockGetRole = vi.fn();
vi.mock("../api/client", () => ({
  createRole: (...args: unknown[]) => mockCreateRole(...args),
  updateRole: (...args: unknown[]) => mockUpdateRole(...args),
  getRole: (...args: unknown[]) => mockGetRole(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: {} }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const existingRole = createRole({
  metadata: { name: "my-role" },
  spec: { apiRules: [{ resources: ["controllers"], verbs: ["get", "list"] }], jenkinsPermissions: ["hudson.model.Hudson.Read"] },
});

// Helper: labels are siblings to their input elements (no htmlFor), so we use nextElementSibling.
function getNameInput(): HTMLInputElement {
  return screen.getByText("Name").nextElementSibling as HTMLInputElement;
}

describe("RoleForm — create mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = undefined;
  });

  it("renders create heading and form fields", () => {
    renderWithProviders(<RoleForm />);
    expect(screen.getByText("Create Role")).toBeInTheDocument();
    expect(getNameInput()).toBeInTheDocument();
    expect(screen.getByText("API Rules")).toBeInTheDocument();
    expect(screen.getByText("Jenkins Permissions (one per line)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("has the name field editable and required", () => {
    renderWithProviders(<RoleForm />);
    const nameInput = getNameInput();
    expect(nameInput).not.toBeDisabled();
    expect(nameInput).toHaveAttribute("required");
  });

  it("submits and navigates to /roles on successful create", async () => {
    mockCreateRole.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleForm />);

    await user.type(getNameInput(), "new-role");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockCreateRole).toHaveBeenCalledWith(
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "new-role" }),
          spec: expect.objectContaining({ apiRules: [] }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/roles");
    });
  });

  it("shows error message when create fails", async () => {
    mockCreateRole.mockRejectedValueOnce(new Error("Already exists"));
    const user = userEvent.setup();
    renderWithProviders(<RoleForm />);

    await user.type(getNameInput(), "duplicate");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText("Already exists")).toBeInTheDocument();
    });
  });

  it("navigates to /roles on Cancel", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleForm />);
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/roles");
  });

  it("adds and removes API rule rows", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleForm />);

    // Initially no API rule rows
    expect(screen.queryAllByRole("listbox").length).toBe(0);

    // Add API rule
    await user.click(screen.getByText("+ Add API Rule"));
    const selects = screen.getAllByRole("listbox");
    expect(selects.length).toBe(2); // resources + verbs

    // Remove the rule
    await user.click(screen.getByText("×")); // × character
    expect(screen.queryAllByRole("listbox").length).toBe(0);
  });

  it("offers all controller lifecycle verbs", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleForm />);
    await user.click(screen.getByText("+ Add API Rule"));
    const verbs = screen.getAllByRole("listbox")[1];
    expect(Array.from(verbs.querySelectorAll("option"), (option) => option.value)).toEqual([
      "*", "read", "create", "update", "delete", "manage", "approve-restart", "approve-deletion",
    ]);
  });
});

describe("RoleForm — edit mode", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = "my-role";
    mockGetRole.mockResolvedValue(existingRole);
  });

  it("renders edit heading and pre-fills fields", async () => {
    renderWithProviders(<RoleForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Role: my-role")).toBeInTheDocument();
    });
    const nameInput = getNameInput();
    expect(nameInput).toBeDisabled();
    expect(nameInput).toHaveValue("my-role");
  });

  it("pre-fills jenkins permissions from existing data", async () => {
    renderWithProviders(<RoleForm />);
    await waitFor(() => {
      expect(screen.getByText("Edit Role: my-role")).toBeInTheDocument();
    });
    const textarea = screen.getByText("Jenkins Permissions (one per line)")
      .closest("div")!
      .querySelector("textarea")!;
    expect(textarea).toHaveValue("hudson.model.Hudson.Read");
  });

  it("submits update and navigates to /roles", async () => {
    mockUpdateRole.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<RoleForm />);

    await waitFor(() => {
      expect(screen.getByText("Edit Role: my-role")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(mockUpdateRole).toHaveBeenCalledWith(
        "my-role",
        expect.objectContaining({
          metadata: expect.objectContaining({ name: "my-role" }),
        }),
      );
      expect(mockNavigate).toHaveBeenCalledWith("/access/roles");
    });
  });
});
