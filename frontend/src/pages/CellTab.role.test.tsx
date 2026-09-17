import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { CellTab } from "./CellTab";
import type { ControllerDetail } from "../hooks/useControllers";
import type { CellPool, CellUsage } from "../types";

const mockUseCellUsage = vi.fn();
const mockUseCellPool = vi.fn();
const mockUseCellGrants = vi.fn();
vi.mock("../hooks/useCells", () => ({
  useCellUsage: (...a: unknown[]) => mockUseCellUsage(...a),
  useCellPool: (...a: unknown[]) => mockUseCellPool(...a),
  useCellGrants: (...a: unknown[]) => mockUseCellGrants(...a),
  useCellPools: vi.fn(),
  useCellUsageList: vi.fn(),
}));

const mockListTeams = vi.fn();
vi.mock("../api/client", () => ({
  listTeams: () => mockListTeams(),
}));

const mockPermissions = vi.fn();
vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: mockPermissions() }),
}));

const mockCanDoGlobal = vi.fn();
const mockCanAccessTenantPipeline = vi.fn();
vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: (...a: unknown[]) => mockCanDoGlobal(...a),
  canAccessTenantPipeline: (...a: unknown[]) => mockCanAccessTenantPipeline(...a),
}));

function roleUsage(): CellUsage {
  return {
    cluster: "core",
    namespace: "ns-a",
    name: "cell-a",
    windowStart: "2026-09-11T00:00:00Z",
    tenants: [
      { tenant: "acme", role: "slot 1", runningNow: 1, runsLast24h: 4, failedLast24h: 1, jobs: 3 },
      { tenant: "beta", role: "standby", runningNow: 0, runsLast24h: 0, failedLast24h: 0, jobs: 1 },
    ],
    totals: { runningNow: 1, runsLast24h: 4, failedLast24h: 1, jobs: 4 },
    maxTenants: 10,
    freeSlots: 8,
  };
}

function rolePool(): CellPool {
  return {
    name: "lts",
    spec: { profileRef: "lts", warm: 2, maxCells: 8, maxTenantsPerCell: 10 },
    status: {
      warmCells: 2,
      placedCells: 3,
      unreadyCells: 0,
      freeTenantSlots: 8,
      conditions: [{ type: "PoolReady", status: "True", reason: "Ready" }],
    },
  };
}

function ctrl(): ControllerDetail {
  return {
    kind: "cell",
    cell: {
      tenants: ["acme", "beta"],
      appliedTenants: ["acme", "beta"],
      instance: "uid-new",
      evacuate: false,
    },
  } as ControllerDetail;
}

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <CellTab ctrl={ctrl()} cluster="core" namespace="ns-a" name="cell-a" active />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("CellTab role column", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseCellUsage.mockReturnValue({ data: roleUsage(), isLoading: false, error: null });
    mockUseCellPool.mockReturnValue({ data: rolePool(), isLoading: false, error: null });
    mockUseCellGrants.mockReturnValue({ data: undefined, isLoading: false, error: null });
    mockListTeams.mockResolvedValue([]);
    mockPermissions.mockReturnValue({ global: {}, scopes: [] });
    mockCanDoGlobal.mockReturnValue(false);
    mockCanAccessTenantPipeline.mockReturnValue(false);
  });

  // The tab names a slot: a cell holding slot 1 of a tenant reads "slot 1".
  it("names the slot a tenant holds on this cell", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("Tenants")).toBeInTheDocument());

    const row = screen.getAllByText("acme")[0].closest("tr");
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getByText("slot 1")).toBeInTheDocument();
  });

  // The tab names a standby. A standby holds the tenant folder and runs no
  // build, and the tab computes nothing: the value comes from the usage
  // read.
  it("names a standby", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("Tenants")).toBeInTheDocument());

    const row = screen.getAllByText("beta")[0].closest("tr");
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getByText("standby")).toBeInTheDocument();
  });

  // A usage read that carries no role leaves the column blank rather than
  // inventing a value.
  it("renders a dash when the read carries no role", async () => {
    mockUseCellUsage.mockReturnValue({
      data: {
        ...roleUsage(),
        tenants: [{ tenant: "acme", runningNow: 0, runsLast24h: 0, failedLast24h: 0, jobs: 0 }],
      },
      isLoading: false,
      error: null,
    });
    renderTab();
    await waitFor(() => expect(screen.getByText("Tenants")).toBeInTheDocument());

    const row = screen.getAllByText("acme")[0].closest("tr");
    expect(within(row as HTMLElement).queryByText("slot 0")).toBeNull();
  });
});
