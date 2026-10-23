import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
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

/** multiSlotTenant holds two active slots and one standby. */
const multiSlotTenant = {
  name: "payments",
  namespaces: ["ns-pay"],
  roleRef: "developer",
  pipelines: { profile: "lts", agentNamespace: "ns-pay", members: 2, standby: 1 },
  conditions: [{ type: "TenantReady", status: "True" }],
  placement: {
    cluster: "core",
    cell: "cell-a",
    cellNamespace: "cells-ns",
    generation: 4,
    members: [
      { cell: "cell-a", cellNamespace: "cells-ns", role: "active", slot: 0, applied: true },
      { cell: "cell-b", cellNamespace: "cells-ns", role: "active", slot: 1, applied: true },
      { cell: "cell-c", cellNamespace: "cells-ns", role: "standby", applied: true },
    ],
  },
};

/** singleSlotTenant carries no member list, which is a tenant the pass has
 * not re-announced since the upgrade. */
const singleSlotTenant = {
  name: "search",
  namespaces: ["ns-search"],
  roleRef: "developer",
  conditions: [{ type: "TenantReady", status: "True" }],
  placement: { cluster: "core", cell: "cell-z", cellNamespace: "cells-ns", generation: 1 },
};

describe("Tenants member list", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // The list names every member, and not slot 0's cell alone.
  it("names every member of a multi-slot tenant", async () => {
    mockListTenants.mockResolvedValue([multiSlotTenant]);
    renderWithProviders(<Tenants />);

    expect(await screen.findByText("cell-a (slot 0)")).toBeInTheDocument();
    expect(screen.getByText("cell-b (slot 1)")).toBeInTheDocument();
    expect(screen.getByText("cell-c (standby)")).toBeInTheDocument();
  });

  // A tenant with no member list falls back to the scalar placement, so it
  // reads exactly as it read before slots existed.
  it("falls back to the scalar cell for a tenant with no member list", async () => {
    mockListTenants.mockResolvedValue([singleSlotTenant]);
    renderWithProviders(<Tenants />);

    expect(await screen.findByText("core/cell-z")).toBeInTheDocument();
  });

  it("keeps each placement member on one line", async () => {
    mockListTenants.mockResolvedValue([multiSlotTenant]);
    renderWithProviders(<Tenants />);

    const member = await screen.findByText("cell-a (slot 0)");
    expect(member.closest("td")?.className).toMatch(/placementCell/);
    expect(member.closest("ul")?.className).toMatch(/memberList/);
  });
});
