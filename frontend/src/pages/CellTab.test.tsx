import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { CellTab } from "./CellTab";
import { ApiError } from "../hooks/useApi";
import type { ControllerDetail } from "../hooks/useControllers";
import type { CellGrants, CellPool, CellUsage } from "../types";

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

function makeCtrl(overrides?: Partial<ControllerDetail>): ControllerDetail {
  return {
    kind: "cell",
    cell: {
      tenants: ["acme", "beta"],
      appliedTenants: ["acme"],
      instance: "uid-new",
      previousInstance: "uid-old",
      evacuate: false,
    },
    ...overrides,
  } as ControllerDetail;
}

function makeUsage(overrides?: Partial<CellUsage>): CellUsage {
  return {
    cluster: "core",
    namespace: "ns-a",
    name: "cell-a",
    windowStart: "2026-09-11T00:00:00Z",
    cellPool: "lts-2-555",
    profile: "lts-2-555",
    tenants: [
      {
        tenant: "acme",
        agentNamespace: "acme-agents",
        placementGeneration: 7,
        runningNow: 1,
        runsLast24h: 4,
        failedLast24h: 1,
        jobs: 3,
      },
      { tenant: "beta", placementGeneration: 2, runningNow: 0, runsLast24h: 0, failedLast24h: 0, jobs: 1 },
    ],
    totals: { runningNow: 1, runsLast24h: 4, failedLast24h: 1, jobs: 4 },
    maxTenants: 10,
    freeSlots: 8,
    queueLength: 2,
    queueLengthAt: "2026-09-12T00:00:00Z",
    ...overrides,
  };
}

function makePool(overrides?: Partial<CellPool>): CellPool {
  return {
    cluster: "core",
    name: "lts-2-555",
    spec: { profileRef: "lts-2-555", warm: 2, maxCells: 8, maxTenantsPerCell: 10 },
    status: {
      warmCells: 2,
      placedCells: 3,
      unreadyCells: 0,
      freeTenantSlots: 8,
      conditions: [{ type: "PoolReady", status: "True", reason: "Ready" }],
    },
    ...overrides,
  };
}

function makeGrants(overrides?: Partial<CellGrants>): CellGrants {
  return {
    cluster: "core",
    namespace: "ns-a",
    cell: "cell-a",
    tenants: [
      {
        tenant: "acme",
        itemRole: "varroa:tenant:acme",
        itemPattern: "^acme($|/.*)$",
        truncated: false,
        grants: [
          {
            source: "tenantScopedBinding",
            binding: "acme-release",
            roleRef: "developer",
            subjects: [{ kind: "Group", name: "acme-devs" }],
            verbs: ["read", "write"],
            jobs: ["release/*"],
          },
        ],
      },
      {
        tenant: "beta",
        itemRole: "varroa:tenant:beta",
        itemPattern: "^beta($|/.*)$",
        truncated: false,
        grants: [],
      },
    ],
    ...overrides,
  };
}

function renderTab(ctrl = makeCtrl()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <CellTab ctrl={ctrl} cluster="core" namespace="ns-a" name="cell-a" active />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mockPermissions.mockReturnValue({ global: {}, scopes: [] });
  mockCanDoGlobal.mockReturnValue(true);
  mockCanAccessTenantPipeline.mockReturnValue(true);
  mockUseCellUsage.mockReturnValue({ data: makeUsage(), isLoading: false, error: null });
  mockUseCellPool.mockReturnValue({ data: makePool(), isLoading: false, error: null });
  mockUseCellGrants.mockReturnValue({ data: makeGrants(), isLoading: false, error: null });
});

describe("Topology panel", () => {
  it("names the pool, the profile, and both instances", () => {
    renderTab();
    expect(screen.getAllByText("lts-2-555").length).toBeGreaterThan(0);
    expect(screen.getByText("uid-new")).toBeInTheDocument();
    expect(screen.getByText("uid-old")).toBeInTheDocument();
  });

  it("states the pool cap and the PoolReady verdict for a pool reader", () => {
    mockUseCellPool.mockReturnValue({
      data: makePool({
        status: {
          warmCells: 0,
          placedCells: 4,
          unreadyCells: 0,
          freeTenantSlots: 0,
          conditions: [{ type: "PoolReady", status: "False", reason: "CellCapReached" }],
        },
      }),
      isLoading: false,
      error: null,
    });
    renderTab();
    expect(screen.getByText("Pool tenant cap")).toBeInTheDocument();
    expect(screen.getByText("False (CellCapReached)")).toBeInTheDocument();
  });

  it("hides the pool values and states the reason without the pool read", () => {
    mockCanDoGlobal.mockImplementation((_p: unknown, resource: string) => resource !== "cellpools");
    renderTab();
    expect(screen.queryByText("Pool tenant cap")).not.toBeInTheDocument();
    expect(screen.getByText(/need the cellpools read grant/)).toBeInTheDocument();
  });

  it("reads the placement state from the applied tenant set", () => {
    renderTab();
    expect(screen.getByText("placed")).toBeInTheDocument();
    expect(screen.getByText("rehydrating")).toBeInTheDocument();
  });

  it("renders the evacuate flag as state and no control", () => {
    renderTab(makeCtrl({ cell: { tenants: ["acme"], appliedTenants: ["acme"], evacuate: true } }));
    expect(screen.getByText("Set by the operator")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /evacuate/i })).not.toBeInTheDocument();
  });

  it("renders the tenant name as text when the caller may not open its pipelines", () => {
    mockCanAccessTenantPipeline.mockReturnValue(false);
    renderTab();
    expect(screen.queryByRole("link", { name: "acme" })).not.toBeInTheDocument();
  });

  it("links a tenant the caller may open", () => {
    renderTab();
    expect(screen.getByRole("link", { name: "acme" })).toHaveAttribute("href", "/pipelines/acme");
  });
});

describe("Capacity and usage panel", () => {
  it("renders the reported cap and free slots", () => {
    renderTab();
    const cap = screen.getByText("Tenant cap").parentElement as HTMLElement;
    expect(within(cap).getByText("10")).toBeInTheDocument();
    const free = screen.getByText("Free slots").parentElement as HTMLElement;
    expect(within(free).getByText("8")).toBeInTheDocument();
  });

  it("renders an em dash and its reason for an absent queue length", () => {
    mockUseCellUsage.mockReturnValue({
      data: makeUsage({ queueLength: undefined, queueLengthAt: undefined }),
      isLoading: false,
      error: null,
    });
    renderTab();
    expect(screen.getByText(/no gauge/)).toBeInTheDocument();
    const row = screen.getByText("Queue length").parentElement as HTMLElement;
    expect(within(row).queryByText("0")).not.toBeInTheDocument();
  });

  it("names both caps when the pool cap differs from the reported cap", () => {
    mockUseCellPool.mockReturnValue({
      data: makePool({ spec: { profileRef: "lts-2-555", maxTenantsPerCell: 4 } }),
      isLoading: false,
      error: null,
    });
    renderTab();
    expect(screen.getByText(/reports a tenant cap of 4/)).toBeInTheDocument();
    expect(screen.getByText(/never the smaller of two values/)).toBeInTheDocument();
  });

  it("names no pool cap for a caller without the pool read", () => {
    mockCanDoGlobal.mockImplementation((_p: unknown, resource: string) => resource !== "cellpools");
    renderTab();
    expect(screen.queryByText(/reports a tenant cap of/)).not.toBeInTheDocument();
    expect(screen.getByText("Tenant cap")).toBeInTheDocument();
  });

  it("labels the counts as record-derived and states the window start", () => {
    renderTab();
    expect(screen.getByText(/come from the build records/)).toBeInTheDocument();
    expect(screen.getByText(/2026-09-11T00:00:00Z/)).toBeInTheDocument();
  });
});

describe("Permissions panel", () => {
  it("lists each grant with its subjects, verbs, and globs", () => {
    renderTab();
    expect(screen.getByText("varroa:tenant:acme")).toBeInTheDocument();
    expect(screen.getByText("^acme($|/.*)$")).toBeInTheDocument();
    expect(screen.getByText("Group:acme-devs")).toBeInTheDocument();
    expect(screen.getByText("read, write")).toBeInTheDocument();
    expect(screen.getByText("release/*")).toBeInTheDocument();
  });

  it("carries no binding control and links the Access page", () => {
    renderTab();
    expect(screen.getByRole("link", { name: "Access page" })).toHaveAttribute(
      "href",
      "/access/role-bindings",
    );
    expect(screen.queryByRole("button", { name: /edit binding/i })).not.toBeInTheDocument();
  });

  it("names the missing verb and lists nothing on a 403", () => {
    mockUseCellGrants.mockReturnValue({
      data: undefined,
      isLoading: false,
      error: new ApiError(403, "forbidden"),
    });
    renderTab();
    expect(screen.getByText(/needs the cluster-wide rolebindings read grant/)).toBeInTheDocument();
    expect(screen.queryByText("varroa:tenant:acme")).not.toBeInTheDocument();
  });

  it("states that no binding reaches a tenant instead of a blank block", () => {
    renderTab();
    expect(screen.getByText(/No binding and no Team membership reaches this tenant/)).toBeInTheDocument();
  });

  it("reports the cap when a tenant's grant list is cut", () => {
    const grants = makeGrants();
    grants.tenants[0].truncated = true;
    mockUseCellGrants.mockReturnValue({ data: grants, isLoading: false, error: null });
    renderTab();
    expect(screen.getByText(/stops at 50 grants/)).toBeInTheDocument();
  });
});

describe("The agent namespace and placement generation columns", () => {
  it("reads both columns from the usage read", () => {
    renderTab();
    expect(screen.getByText("Agent namespace")).toBeInTheDocument();
    expect(screen.getByText("Placement generation")).toBeInTheDocument();
    const acme = screen.getByRole("link", { name: "acme" }).closest("tr") as HTMLElement;
    expect(within(acme).getByText("acme-agents")).toBeInTheDocument();
    expect(within(acme).getByText("7")).toBeInTheDocument();
  });

  it("renders a dash when the usage row carries no agent namespace", () => {
    renderTab();
    const beta = screen.getByRole("link", { name: "beta" }).closest("tr") as HTMLElement;
    expect(within(beta).getByText("2")).toBeInTheDocument();
    expect(within(beta).getAllByText("—").length).toBeGreaterThan(0);
  });

  it("renders both columns for a caller without the admin grant", () => {
    mockCanDoGlobal.mockImplementation((_p: unknown, resource: string) => resource !== "*");
    renderTab();
    expect(screen.getByText("Agent namespace")).toBeInTheDocument();
    expect(screen.getByText("acme-agents")).toBeInTheDocument();
  });
});
