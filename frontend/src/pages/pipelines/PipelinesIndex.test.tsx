import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Routes, Route, useParams, useLocation } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ToastProvider } from "../../components/Toast";
import { ApiError } from "../../hooks/useApi";
import type { FleetRunPage, FleetRunRow, PipelinesOverview, PipelinesOverviewTenant } from "../../types";
import PipelinesIndex from "./PipelinesIndex";

const mockGetMyPermissions = vi.fn();
const mockGetPipelinesOverview = vi.fn();
const mockListFleetRuns = vi.fn();
vi.mock("../../api/client", () => ({
  getMyPermissions: (...args: unknown[]) => mockGetMyPermissions(...args),
  getPipelinesOverview: (...args: unknown[]) => mockGetPipelinesOverview(...args),
  listFleetRuns: (...args: unknown[]) => mockListFleetRuns(...args),
  rerunRun: vi.fn(),
  cancelRun: vi.fn(),
  fleetRunStreamUrl: () => "/api/v1/pipelines/runs/stream",
}));
vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: () => ({ lastEvent: null, readyState: "closed", error: null }),
}));
const mockAssistant = { available: false };
vi.mock("../../components/assistant/AssistantProvider", () => ({
  useAssistant: () => mockAssistant,
}));
vi.mock("./ProposePipelineCard", () => ({
  default: ({ tenant }: { tenant: string }) => <div>propose for {tenant}</div>,
}));

function TenantPage() {
  const { tenant } = useParams();
  return <div>tenant page: {tenant}</div>;
}

function LocationProbe() {
  const location = useLocation();
  return <div>at {location.pathname}</div>;
}

function renderPage() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={["/pipelines"]}>
      <QueryClientProvider client={queryClient}>
        <ToastProvider>
          <Routes>
            <Route
              path="/pipelines"
              element={
                <>
                  <PipelinesIndex />
                  <LocationProbe />
                </>
              }
            />
            <Route path="/pipelines/:tenant" element={<TenantPage />} />
          </Routes>
        </ToastProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
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

function overview(tenants: PipelinesOverviewTenant[], over: Partial<PipelinesOverview> = {}): PipelinesOverview {
  return {
    windowStart: "2026-10-29T12:00:00Z",
    localCluster: "core",
    clusters: 2,
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
    ...over,
  };
}

function run(over: Partial<FleetRunRow> = {}): FleetRunRow {
  return {
    runId: "r1",
    status: "COMPLETED",
    result: "SUCCESS",
    cluster: "core",
    tenant: "pay",
    tenantFolder: "pay",
    jobPath: "pay/api",
    buildNumber: 3,
    actor: "nate",
    branch: "main",
    title: "Fix the parser",
    trigger: { kind: "push", label: "Push" },
    pendingInput: false,
    canWrite: false,
    createdAt: new Date().toISOString(),
    ...over,
  };
}

function page(items: FleetRunRow[]): FleetRunPage {
  return { items };
}

const readAll = {
  global: {},
  scopes: [],
  tenants: [
    { tenant: "pay", jobs: [], capabilities: { pipelines: { read: true } } },
    { tenant: "ops", jobs: [], capabilities: { pipelines: { read: true } } },
  ],
};

describe("PipelinesIndex", () => {
  beforeEach(() => {
    mockGetMyPermissions.mockReset();
    mockGetPipelinesOverview.mockReset();
    mockListFleetRuns.mockReset();
    mockAssistant.available = false;
    mockGetMyPermissions.mockResolvedValue(readAll);
    mockListFleetRuns.mockResolvedValue(page([run()]));
  });

  it("sets the document title to Pipelines while the overview loads", () => {
    mockGetPipelinesOverview.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(document.title).toBe("Pipelines");
    expect(screen.getByText("Loading pipelines…")).toBeInTheDocument();
  });

  it("renders the strip, counters, rail, and feed for two tenants", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("ops"), tenantRow("pay")]));
    renderPage();
    expect(await screen.findByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
    expect(screen.getByText("2 tenants you can read · 2 clusters · runs from every tenant, newest first")).toBeInTheDocument();
    expect(screen.getByText("Nothing needs attention.")).toBeInTheDocument();
    expect(screen.getByText("Pass rate")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^pay/ })).toBeInTheDocument();
    expect(await screen.findByText("Fix the parser")).toBeInTheDocument();
  });

  it("does not redirect for one tenant and selects it in the rail", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("acme")]));
    renderPage();
    expect(await screen.findByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
    expect(screen.queryByText("tenant page: acme")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^acme/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("at /pipelines")).toBeInTheDocument();
    await waitFor(() => {
      expect(mockListFleetRuns).toHaveBeenCalledWith(expect.objectContaining({ tenant: ["acme"] }));
    });
  });

  it("shows No pipelines access with no tenant", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview([]));
    renderPage();
    expect(await screen.findByText("No pipelines access")).toBeInTheDocument();
  });

  it("a feed error keeps the rail and counters", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("ops"), tenantRow("pay")]));
    mockListFleetRuns.mockRejectedValue(new Error("boom"));
    renderPage();
    expect(await screen.findByText("Unable to load runs")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^pay/ })).toBeInTheDocument();
    expect(screen.getByText("Pass rate")).toBeInTheDocument();
  });

  it("New pipeline opens the panel for a writable tenant", async () => {
    const user = userEvent.setup();
    mockAssistant.available = true;
    mockGetMyPermissions.mockResolvedValue({
      global: {},
      scopes: [],
      tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true, write: true } } }],
    });
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("acme")]));
    renderPage();
    await user.click(await screen.findByRole("button", { name: "New pipeline" }));
    expect(await screen.findByText("propose for acme")).toBeInTheDocument();
  });

  it("explains a disabled record store on 503", async () => {
    mockGetPipelinesOverview.mockRejectedValue(new ApiError(503, "record store is off"));
    renderPage();
    expect(await screen.findByText("Pipeline data is off")).toBeInTheDocument();
  });

  it("explains an unreachable record store on a 503 with the unreachable code", async () => {
    mockGetPipelinesOverview.mockRejectedValue(
      new ApiError(503, "record store unreachable", { error: "record store unreachable", code: "record_store_unreachable" }),
    );
    renderPage();
    expect(await screen.findByText("Pipeline data is unreachable")).toBeInTheDocument();
    expect(screen.queryByText("Pipeline data is off")).not.toBeInTheDocument();
  });

  it("shows an error with Retry when the overview fails", async () => {
    mockGetPipelinesOverview.mockRejectedValue(new Error("boom"));
    renderPage();
    const retry = await screen.findByRole("button", { name: "Retry" });
    mockGetPipelinesOverview.mockResolvedValue(overview([tenantRow("ops")]));
    await userEvent.setup().click(retry);
    expect(await screen.findByRole("heading", { name: "Pipelines" })).toBeInTheDocument();
    expect(within(screen.getByRole("navigation", { name: "Tenants" })).getByRole("button", { name: /^ops/ })).toBeInTheDocument();
  });
});
