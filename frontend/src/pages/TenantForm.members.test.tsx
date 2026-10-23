import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import TenantForm from "./TenantForm";

const mockNavigate = vi.fn();
let mockParams: { name?: string } = {};
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useNavigate: () => mockNavigate,
    useParams: () => mockParams,
  };
});

const mockGetTenant = vi.fn();
const mockCreateTenant = vi.fn();
const mockUpdateTenant = vi.fn();
const mockListRoles = vi.fn();
const mockListComposedBundles = vi.fn();
vi.mock("../api/client", () => ({
  getTenant: (...args: unknown[]) => mockGetTenant(...args),
  createTenant: (...args: unknown[]) => mockCreateTenant(...args),
  updateTenant: (...args: unknown[]) => mockUpdateTenant(...args),
  listRoles: (...args: unknown[]) => mockListRoles(...args),
  listComposedBundles: (...args: unknown[]) => mockListComposedBundles(...args),
}));

vi.mock("../hooks/useClusters", () => ({
  useClusters: () => ({ data: [{ name: "core", core: true }] }),
}));

/** placedTenant holds two active slots and one standby. */
const placedTenant = {
  name: "payments",
  members: ["alice"],
  namespaces: ["ns-pay"],
  roleRef: "developer",
  pipelines: { profile: "lts", agentNamespace: "ns-pay", members: 2, standby: 1 },
  conditions: [{ type: "Placed", status: "True", reason: "MembersApplied" }],
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

describe("TenantForm member list", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams = { name: "payments" };
    mockListRoles.mockResolvedValue({ items: [{ metadata: { name: "developer" } }] });
    mockListComposedBundles.mockResolvedValue({ items: [] });
  });

  // The page names every member, so a multi-slot tenant shows every cell
  // and not slot 0's alone.
  it("names every member of a multi-slot tenant", async () => {
    mockGetTenant.mockResolvedValue(placedTenant);
    render(<TenantForm />);

    expect(
      await screen.findByText("cell-a (slot 0), cell-b (slot 1), cell-c (standby)"),
    ).toBeInTheDocument();
  });

  // A tenant with no member list renders no member line at all, so the
  // scalar cell line stands on its own.
  it("renders no member line for a tenant with no member list", async () => {
    mockGetTenant.mockResolvedValue({
      ...placedTenant,
      placement: { cluster: "core", cell: "cell-a", cellNamespace: "cells-ns", generation: 1 },
    });
    render(<TenantForm />);

    expect(await screen.findByText(/core\/cells-ns\/cell-a/)).toBeInTheDocument();
    expect(screen.queryByText("Members")).toBeNull();
  });
});
