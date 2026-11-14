import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import RoleBindingForm from "./RoleBindingForm";
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
vi.mock("../api/client", () => ({
  listRoles: (...args: unknown[]) => mockListRoles(...args),
  getRoleBinding: (...args: unknown[]) => mockGetRoleBinding(...args),
  createRoleBinding: (...args: unknown[]) => mockCreateRoleBinding(...args),
  updateRoleBinding: (...args: unknown[]) => mockUpdateRoleBinding(...args),
  listTenants: vi.fn().mockResolvedValue([]),
  listPipelines: vi.fn().mockResolvedValue({ items: [], runningNow: [], tenantFolder: "", truncated: false }),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: {} }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const operatorBinding = createRoleBinding({
  metadata: {
    name: "broodschedule-de59698b5963b41984ed191b495eeae7fd55f8d8a1eb7876",
    annotations: {
      "varroa.dev/broodschedule-namespace": "ns-a",
      "varroa.dev/broodschedule-name": "nightly",
    },
  },
  spec: {
    subjects: [{ kind: "User", name: "schedule:ns-a/nightly" }],
    roleRef: "operator",
    scope: { namespaces: ["ns-a"] },
  },
});

describe("RoleBindingForm — an operator-made binding", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams.name = operatorBinding.metadata.name;
    mockListRoles.mockResolvedValue({ items: [createRole({ metadata: { name: "operator" } })] });
    mockGetRoleBinding.mockResolvedValue(operatorBinding);
  });

  it("shows the note with a link to the owner", async () => {
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByRole("note")).toBeInTheDocument();
    });
    const link = screen.getByRole("link", { name: "ns-a/nightly" });
    expect(link).toHaveAttribute("href", "/brood-schedules/ns-a/nightly");
  });

  it("titles the page as a read-only binding", async () => {
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByText(`Role Binding: ${operatorBinding.metadata.name}`)).toBeInTheDocument();
    });
  });

  it("hides Save and shows Back, which navigates to the list", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Back" })).toBeInTheDocument();
    });
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(mockNavigate).toHaveBeenCalledWith("/access/role-bindings");
  });

  it("disables the inputs and never calls updateRoleBinding", async () => {
    renderWithProviders(<RoleBindingForm />);
    await waitFor(() => {
      expect(screen.getByRole("note")).toBeInTheDocument();
    });
    const inputs = document.querySelectorAll("fieldset input, fieldset select");
    expect(inputs.length).toBeGreaterThan(0);
    inputs.forEach((el) => expect(el).toBeDisabled());
    expect(mockUpdateRoleBinding).not.toHaveBeenCalled();
  });
});
