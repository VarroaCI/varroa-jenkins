import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Dashboard from "./Dashboard";
import { renderWithProviders } from "../test/render-utils";
import { ApiError } from "../hooks/useApi";
import type { Permissions } from "../types/auth";
import type { PipelinesOverview, PipelinesOverviewTenant } from "../types";

const mockUseAuth = vi.fn();
vi.mock("../context/AuthContext", () => ({
  useAuth: () => mockUseAuth(),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const mockUseControllers = vi.fn();
const mockFanoutFailures = vi.fn((): { name: string; ok: boolean; error?: string }[] => []);
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
  useControllerFanoutFailures: () => ({ data: mockFanoutFailures() }),
}));

const mockUseClusters = vi.fn(() => ({ data: undefined, isLoading: false, error: null }));
vi.mock("../hooks/useClusters", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useClusters")>()),
  useClusters: () => mockUseClusters(),
}));

vi.mock("../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useApi")>()),
  bffFetch: vi.fn().mockResolvedValue({ items: [] }),
}));

const mockGetPipelinesOverview = vi.fn();
const mockListFleetRuns = vi.fn();
vi.mock("../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/client")>()),
  getPipelinesOverview: (...args: unknown[]) => mockGetPipelinesOverview(...args),
  listFleetRuns: (...args: unknown[]) => mockListFleetRuns(...args),
  getUpdateCenterStatus: vi.fn().mockResolvedValue({ enabled: false, conditions: [], gaps: [] }),
  fleetRunStreamUrl: () => "/api/v1/pipelines/runs/stream",
}));

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: () => ({ lastEvent: null, readyState: "closed", error: null }),
}));

vi.mock("./ConnectClientTile", async () => {
  const { createElement } = await import("react");
  return {
    ConnectClientTile: () => createElement("div", { "data-testid": "connect-client-tile" }),
  };
});

const PIPELINES: Permissions = { global: { pipelines: { read: true } }, scopes: [] };
const CONTROLLERS: Permissions = { global: { controllers: { read: true } }, scopes: [] };
const BOTH: Permissions = { global: { pipelines: { read: true }, controllers: { read: true } }, scopes: [] };
const NEITHER: Permissions = { global: {}, scopes: [] };

function auth(over: Partial<Record<string, unknown>> = {}) {
  return {
    permissions: BOTH,
    permissionsLoading: false,
    permissionsError: null,
    retryPermissions: vi.fn(),
    ...over,
  };
}

function tenantRow(name: string, over: Partial<PipelinesOverviewTenant> = {}): PipelinesOverviewTenant {
  return {
    name,
    clusters: ["core"],
    placement: "placed",
    runs24h: 1,
    failed24h: 0,
    running: 0,
    waitingInput: 0,
    locksHeld: 0,
    recent: [],
    ...over,
  };
}

function overview(tenants: PipelinesOverviewTenant[]): PipelinesOverview {
  return {
    windowStart: "2026-10-29T12:00:00Z",
    localCluster: "core",
    clusters: 1,
    canReadFleetLocks: false,
    totals: {
      runs24h: 12,
      runsPrior24h: 9,
      passed: 3,
      failed: 1,
      interrupted: 0,
      finished: 4,
      running: 1,
      queued: 0,
      passRate: 0.75,
      medianDurationMs: 60000,
      p90DurationMs: 120000,
    },
    attention: { failed24h: 0, waitingInput: 0, awaitingCell: 0, locksHeld: 0 },
    tenants,
  };
}

function controller(name: string, namespace = "varroa", cluster = "core", over: Record<string, unknown> = {}) {
  return { name, cluster, namespace, phase: "Running", miteConnected: true, endpoint: "", ...over };
}

function cluster(name: string, core: boolean) {
  return {
    name,
    healthy: true,
    core,
    lastHeartbeat: new Date().toISOString(),
    operatorVersion: "1.0.0",
    k8sVersion: "1.30.0",
    controllerCount: 3,
    connectedCount: 2,
    state: "active" as const,
  };
}

describe("Dashboard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseAuth.mockReturnValue(auth());
    mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("pay")]));
    mockListFleetRuns.mockResolvedValue({ items: [], nextCursor: undefined });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("titles the page Overview", async () => {
    renderWithProviders(<Dashboard />);
    expect(screen.getByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
  });

  it("shows Loading... while the permissions load", () => {
    mockUseAuth.mockReturnValue(auth({ permissions: undefined, permissionsLoading: true }));
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("Loading...")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Pipelines" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Controllers" })).toBeNull();
  });

  it("renders Pipelines before Controllers for a user who can read both", async () => {
    renderWithProviders(<Dashboard />);
    const pipelines = await screen.findByRole("heading", { name: "Pipelines" });
    const controllers = screen.getByRole("heading", { name: "Controllers" });
    expect(pipelines.compareDocumentPosition(controllers) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("renders only the Pipelines section for a user with pipelines access and no controller access", async () => {
    mockUseAuth.mockReturnValue(auth({ permissions: PIPELINES }));
    renderWithProviders(<Dashboard />);
    expect(await screen.findByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Controllers" })).toBeNull();
  });

  it("renders only the Controllers section for a user with no pipelines access, and never reads the overview", async () => {
    mockUseAuth.mockReturnValue(auth({ permissions: CONTROLLERS }));
    renderWithProviders(<Dashboard />);
    expect(await screen.findByRole("heading", { name: "Controllers" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Pipelines" })).toBeNull();
    expect(mockGetPipelinesOverview).not.toHaveBeenCalled();
  });

  it("explains a user with neither access", () => {
    mockUseAuth.mockReturnValue(auth({ permissions: NEITHER }));
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("No access")).toBeInTheDocument();
    expect(
      screen.getByText("No role gives you access to pipelines or controllers. Ask an administrator to add you."),
    ).toBeInTheDocument();
    expect(mockGetPipelinesOverview).not.toHaveBeenCalled();
  });

  it("shows Retry when the permissions read fails", async () => {
    const retry = vi.fn();
    mockUseAuth.mockReturnValue(auth({ permissions: undefined, permissionsError: new Error("boom"), retryPermissions: retry }));
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("Unable to load your access")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("names both counts for a user who can read both", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("pay"), tenantRow("ops")]));
    mockUseControllers.mockReturnValue({
      data: [controller("a"), controller("b"), controller("c", "other")],
      isLoading: false,
      error: null,
    });
    renderWithProviders(<Dashboard />);
    expect(
      await screen.findByText("2 tenants · 3 controllers across 2 namespaces"),
    ).toBeInTheDocument();
  });

  it("uses singular nouns for one tenant and one controller", async () => {
    mockUseControllers.mockReturnValue({ data: [controller("a")], isLoading: false, error: null });
    renderWithProviders(<Dashboard />);
    expect(await screen.findByText("1 tenant · 1 controller across 1 namespace")).toBeInTheDocument();
  });

  it("counts (cluster, namespace) identities, not bare namespace names", async () => {
    mockUseControllers.mockReturnValue({
      data: [
        // Same namespace name in two clusters must count as two.
        controller("ctrl-a", "varroa", "core"),
        controller("ctrl-b", "varroa", "cluster-b"),
        controller("ctrl-c", "varroa", "core"),
      ],
      isLoading: false,
      error: null,
    });
    renderWithProviders(<Dashboard />);
    expect(await screen.findByText(/3 controllers across 2 namespaces/)).toBeInTheDocument();
  });

  it("names only the tenant count for a pipelines-only user", async () => {
    mockUseAuth.mockReturnValue(auth({ permissions: PIPELINES }));
    renderWithProviders(<Dashboard />);
    expect(await screen.findByText("1 tenant")).toBeInTheDocument();
  });

  it("leaves out the tenant count when the overview fails", async () => {
    mockGetPipelinesOverview.mockRejectedValue(new ApiError(500, "boom"));
    mockUseControllers.mockReturnValue({ data: [controller("a")], isLoading: false, error: null });
    renderWithProviders(<Dashboard />);
    const sub = await screen.findByText("1 controller across 1 namespace");
    expect(sub.textContent).not.toMatch(/tenant/);
  });

  it("a controllers error keeps the pipelines section", async () => {
    mockUseControllers.mockReturnValue({ data: null, isLoading: false, error: { message: "Connection refused" } });
    renderWithProviders(<Dashboard />);
    expect(await screen.findByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
    expect(screen.getByText(/Failed to load brood data: Connection refused/)).toBeInTheDocument();
  });

  it("a record store 503 keeps the controllers section", async () => {
    mockGetPipelinesOverview.mockRejectedValue(new ApiError(503, "down"));
    renderWithProviders(<Dashboard />);
    expect(await screen.findByText("Pipeline data is off")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Controllers" })).toBeInTheDocument();
  });

  it("a render error in the Controllers section keeps the Pipelines section", async () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    mockUseControllers.mockReturnValue({
      data: [controller("a", "varroa", "core", { attention: { kind: "NoSuchKind" } })],
      isLoading: false,
      error: null,
    });
    renderWithProviders(<Dashboard />);
    expect(await screen.findByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
    spy.mockRestore();
  });

  it("renders the connect tile after the last section", async () => {
    renderWithProviders(<Dashboard />);
    const tile = await screen.findByTestId("connect-client-tile");
    const controllersHeading = screen.getByRole("heading", { name: "Controllers" });
    expect(controllersHeading.compareDocumentPosition(tile) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("renders the connect tile after the Pipelines section for a pipelines-only user", async () => {
    mockUseAuth.mockReturnValue(auth({ permissions: PIPELINES }));
    renderWithProviders(<Dashboard />);
    const tile = await screen.findByTestId("connect-client-tile");
    const pipelines = await screen.findByRole("heading", { name: "Pipelines" });
    expect(pipelines.compareDocumentPosition(tile) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("marks the strip card of a cluster whose list failed as list unavailable", async () => {
    mockUseClusters.mockReturnValue({
      data: [cluster("core", true), cluster("member-b", false)],
      isLoading: false,
      error: null,
    } as never);
    mockFanoutFailures.mockReturnValue([{ name: "member-b", ok: false, error: "cluster unreachable" }]);
    renderWithProviders(<Dashboard />);
    const card = (await screen.findByText("list unavailable")).closest("a");
    expect(card).not.toBeNull();
    expect(within(card as HTMLElement).getByText("member-b")).toBeInTheDocument();
  });
});
