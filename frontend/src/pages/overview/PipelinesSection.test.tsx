import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PipelinesSection } from "./PipelinesSection";
import { renderWithProviders } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import type { FleetRunPage, FleetRunRow, PipelinesOverview, PipelinesOverviewTenant } from "../../types";

const mockGetPipelinesOverview = vi.fn();
const mockListFleetRuns = vi.fn();
vi.mock("../../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/client")>()),
  getPipelinesOverview: (...args: unknown[]) => mockGetPipelinesOverview(...args),
  listFleetRuns: (...args: unknown[]) => mockListFleetRuns(...args),
  rerunRun: vi.fn(),
  cancelRun: vi.fn(),
  fleetRunStreamUrl: () => "/api/v1/pipelines/runs/stream",
}));

vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: () => ({ lastEvent: null, readyState: "closed", error: null }),
}));

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

function overview(tenants: PipelinesOverviewTenant[], over: Partial<PipelinesOverview> = {}): PipelinesOverview {
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
    attention: { failed24h: 2, waitingInput: 0, awaitingCell: 0, locksHeld: 1 },
    tenants,
    ...over,
  };
}

function run(id: number): FleetRunRow {
  return {
    runId: `r${id}`,
    status: "COMPLETED",
    result: "SUCCESS",
    cluster: "core",
    tenant: "pay",
    tenantFolder: "pay",
    jobPath: "pay/api",
    buildNumber: id,
    actor: "nate",
    branch: "main",
    changeSummary: `Change ${id}`,
    trigger: { kind: "push", label: "Push" },
    createdAt: "2026-10-29T11:00:00Z",
    queuedAt: "2026-10-29T11:00:00Z",
    startedAt: "2026-10-29T11:00:01Z",
    completedAt: "2026-10-29T11:01:00Z",
    canWrite: false,
  } as unknown as FleetRunRow;
}

function feedPage(items: FleetRunRow[]): FleetRunPage {
  return { items, nextCursor: undefined } as unknown as FleetRunPage;
}

describe("PipelinesSection", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("pay", { runs24h: 4 })]));
    mockListFleetRuns.mockResolvedValue(feedPage([run(1)]));
  });

  it("links the section head to /pipelines", async () => {
    renderWithProviders(<PipelinesSection />);
    const link = await screen.findByRole("link", { name: /Open pipelines/ });
    expect(link.getAttribute("href")).toBe("/pipelines");
    expect(screen.getByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
  });

  it("renders the runs, pass rate, running, and needs-attention tiles", async () => {
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("Runs")).toBeInTheDocument();
    expect(screen.getByText("Pass rate")).toBeInTheDocument();
    expect(screen.getByText("Running now")).toBeInTheDocument();
    expect(screen.getByText("Needs attention")).toBeInTheDocument();
    expect(screen.getByText("2 failed · 1 lock held")).toBeInTheDocument();
  });

  it("lists at most eight tenants in BFF order and links each to its tenant home", async () => {
    const tenants = Array.from({ length: 10 }, (_, i) => tenantRow(`t${i}`));
    mockGetPipelinesOverview.mockResolvedValue(overview(tenants));
    renderWithProviders(<PipelinesSection />);
    const nav = await screen.findByRole("navigation", { name: "Tenants" });
    const links = within(nav).getAllByRole("link");
    expect(links).toHaveLength(8);
    expect(links[0].getAttribute("href")).toBe("/pipelines/t0");
    expect(links[7].getAttribute("href")).toBe("/pipelines/t7");
    expect(screen.getByText(/and 2 more/)).toBeInTheDocument();
  });

  it("lists the five newest runs and links each to its build page", async () => {
    mockListFleetRuns.mockResolvedValue(feedPage([1, 2, 3, 4, 5, 6].map(run)));
    renderWithProviders(<PipelinesSection />);
    const link = await screen.findByRole("link", { name: "Change 1" });
    expect(link.getAttribute("href")).toContain("r1");
    expect(screen.queryByRole("link", { name: "Change 6" })).toBeNull();
    expect(screen.getAllByRole("row")).toHaveLength(6);
  });

  it("reads the default fleet feed view", async () => {
    renderWithProviders(<PipelinesSection />);
    await screen.findByRole("link", { name: "Change 1" });
    expect(mockListFleetRuns).toHaveBeenCalled();
    const params = mockListFleetRuns.mock.calls[0][0];
    expect(params.tenant ?? []).toEqual([]);
    expect(params.sort).toBe("started");
  });

  it("says No runs yet when the feed is empty", async () => {
    mockListFleetRuns.mockResolvedValue(feedPage([]));
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("No runs yet.")).toBeInTheDocument();
  });

  it("shows Retry when the run feed fails", async () => {
    mockListFleetRuns.mockRejectedValue(new ApiError(500, "boom"));
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("Unable to load runs")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("explains a disabled record store on 503 without Back or Home", async () => {
    mockGetPipelinesOverview.mockRejectedValue(new ApiError(503, "off"));
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("Pipeline data is off")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Back" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Home" })).toBeNull();
  });

  it("explains an unreachable record store and Retry reads again", async () => {
    mockGetPipelinesOverview.mockRejectedValue(
      new ApiError(503, "unreachable", { code: "record_store_unreachable" }),
    );
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("Pipeline data is unreachable")).toBeInTheDocument();
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("pay")]));
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByText("Runs")).toBeInTheDocument());
  });

  it("shows Retry when the overview fails", async () => {
    mockGetPipelinesOverview.mockRejectedValue(new ApiError(500, "boom"));
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("Unable to load pipelines")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("shows No pipelines access with no tenant and reads no runs", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview([]));
    renderWithProviders(<PipelinesSection />);
    expect(await screen.findByText("No pipelines access")).toBeInTheDocument();
    expect(mockListFleetRuns).not.toHaveBeenCalled();
  });

  it("shows skeleton tiles while the overview loads", () => {
    mockGetPipelinesOverview.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<PipelinesSection />);
    expect(screen.getAllByTestId("metric-skeleton")).toHaveLength(4);
  });
});
