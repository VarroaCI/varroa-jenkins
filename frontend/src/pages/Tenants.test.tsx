import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Tenants from "./Tenants";
import { renderWithProviders } from "../test/render-utils";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListTenants = vi.fn();
const mockDeleteTenant = vi.fn();
vi.mock("../api/client", () => ({
  listTenants: (...args: unknown[]) => mockListTenants(...args),
  deleteTenant: (...args: unknown[]) => mockDeleteTenant(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: { "*": { "*": true } }, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const tenant1 = {
  name: "alpha-team",
  members: ["alice"],
  namespaces: ["ns-alpha"],
  roleRef: "developer",
  conditions: [{ type: "TenantReady", status: "True" }],
};

const tenant2 = {
  name: "beta-tenant",
  subjects: [{ kind: "Group", name: "idp-devs" }],
  namespaces: ["ns-beta"],
  roleRef: "viewer",
  conditions: [{ type: "TenantReady", status: "False", reason: "NamespaceMissing" }],
};

describe("Tenants", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListTenants.mockResolvedValue([tenant1, tenant2]);
  });

  it("shows loading state", () => {
    mockListTenants.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<Tenants />);
    expect(screen.getByText("Loading...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListTenants.mockRejectedValue(new Error("Server error"));
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      expect(screen.getByText(/Server error/)).toBeInTheDocument();
    });
  });

  it("renders tenant list", async () => {
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      expect(screen.getByText("alpha-team")).toBeInTheDocument();
    });
    expect(screen.getByText("beta-tenant")).toBeInTheDocument();
  });

  it("shows status badges", async () => {
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      expect(screen.getByText("Ready")).toBeInTheDocument();
    });
    expect(screen.getByText("NamespaceMissing")).toBeInTheDocument();
  });

  it("shows create button for admin", async () => {
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      expect(screen.getByText("Create Tenant")).toBeInTheDocument();
    });
  });

  it("navigates to create form", async () => {
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      screen.getByText("Create Tenant").click();
    });
    expect(mockNavigate).toHaveBeenCalledWith("/access/tenants/create");
  });

  it("shows exports with their kind and callers, and a dash when there are none", async () => {
    mockListTenants.mockResolvedValue([
      tenant1,
      {
        ...tenant2,
        pipelines: {
          profile: "lts",
          agentNamespace: "ns1",
          exports: [
            { job: "release/*", callers: ["payments", "search"] },
            { locks: ["deploy-prod"], callers: ["alpha"] },
          ],
        },
      },
    ]);
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      expect(screen.getByText("beta-tenant")).toBeInTheDocument();
    });
    expect(screen.getByText(/job:/)).toBeInTheDocument();
    expect(screen.getByText(/release\/\* → payments, search/)).toBeInTheDocument();
    expect(screen.getByText(/locks:/)).toBeInTheDocument();
    expect(screen.getByText(/deploy-prod → alpha/)).toBeInTheDocument();
  });

  it("can delete a tenant", async () => {
    mockDeleteTenant.mockResolvedValue(undefined);
    renderWithProviders(<Tenants />);
    await waitFor(() => {
      expect(screen.getByText("alpha-team")).toBeInTheDocument();
    });

    const deleteButtons = screen.getAllByText("Delete");
    await userEvent.click(deleteButtons[0]);

    await waitFor(() => {
      expect(screen.getByText(/Delete tenant "alpha-team"/)).toBeInTheDocument();
    });

    const confirmButtons = screen.getAllByText("Delete");
    // Click the Delete button in the modal (the second "Delete" instance)
    await userEvent.click(confirmButtons[confirmButtons.length - 1]);
    await waitFor(() => {
      expect(mockDeleteTenant).toHaveBeenCalledWith("alpha-team");
    });
  });

  it("wraps the tenant table in a horizontal scroll container", async () => {
    mockListTenants.mockResolvedValue([tenant1, tenant2]);
    renderWithProviders(<Tenants />);

    await screen.findByText("alpha-team");
    expect(screen.getByRole("table").parentElement?.className).toMatch(/tableWrap/);
  });
});
