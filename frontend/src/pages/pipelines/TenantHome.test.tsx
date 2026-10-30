import fs from "fs";
import path from "path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Routes, Route, useLocation, useNavigate } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import { createFleetLock, createPipelineSummary, createRunningRun } from "../../test/factories";
import type { RunNotification } from "../../types";
import TenantHome from "./TenantHome";

const mockListPipelines = vi.fn();
const mockListRunners = vi.fn();
const mockGetTenantCell = vi.fn();
const mockListFleetLocks = vi.fn();
const mockGetTenant = vi.fn();
const mockGetMyPermissions = vi.fn();
// mockListClusters answers the core cluster by default, so a core-placed
// tenant shows no member chip.
const mockListClusters = vi.fn(async () => [{ name: "core", core: true }]);
// mockAssistant is the assistant state the proposal card reads.
const mockAssistant = { available: false, switchOn: false };
vi.mock("../../components/assistant/AssistantProvider", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../components/assistant/AssistantProvider")>()),
  useAssistant: () => mockAssistant,
}));
vi.mock("./ProposePipelineCard", () => ({
  default: () => <div>propose-card</div>,
}));
vi.mock("../../api/client", () => ({
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
  getTenant: (...args: unknown[]) => mockGetTenant(...args),
  getMyPermissions: (...args: unknown[]) => mockGetMyPermissions(...args),
  listRunners: (...args: unknown[]) => mockListRunners(...args),
  getTenantCell: (...args: unknown[]) => mockGetTenantCell(...args),
  listFleetLocks: (...args: unknown[]) => mockListFleetLocks(...args),
  listClusters: () => mockListClusters(),
  tenantRunStreamUrl: (tenant: string) => `/api/v1/pipelines/${encodeURIComponent(tenant)}/stream`,
}));

/** RunEvent holds one frame the page reads off the tenant stream. */
type RunEvent = { type: string; data: RunNotification };

const mockUseEventStream = vi.fn(
  (_baseUrl?: string | null, _scope?: string | null, _eventNames?: string[]) => ({
    lastEvent: null as RunEvent | null,
    readyState: "closed",
    error: null as Error | null,
  }),
);
vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: (...args: Parameters<typeof mockUseEventStream>) => mockUseEventStream(...args),
}));

/** notification builds one stream frame of a tenant's run. */
function notification(overrides: Partial<RunNotification> = {}): RunNotification {
  return {
    kind: "run",
    cluster: "core",
    tenant: "payments",
    job_path: "payments/checkout-api",
    run_id: "run-1",
    seq: 1,
    at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  mockUseEventStream.mockClear();
  mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
  mockGetTenant.mockReset();
  mockGetMyPermissions.mockReset();
  mockGetMyPermissions.mockResolvedValue({ global: {}, scopes: [], tenants: [] });
  mockAssistant.available = false;
  mockAssistant.switchOn = false;
});

/** LocationSpy publishes the current search string, so a test can read the
 * URL the page wrote. */
function LocationSpy() {
  const location = useLocation();
  return <div data-testid="search">{location.search}</div>;
}

/** renderPage returns repaint, which re-renders the page so it reads the
 * stream state the test just set. */
function renderPage(route = "/pipelines/acme") {
  const queryClient = createTestQueryClient();
  const tree = () => (
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <LocationSpy />
        <Routes>
          <Route path="/pipelines/:tenant" element={<TenantHome />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree());
  return { ...view, repaint: () => view.rerender(tree()) };
}

/** BackControl steps the router's own history back, which is what the
 * browser back button does. */
function BackControl() {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(-1)}>
      Step back
    </button>
  );
}

/** renderWithHistory renders the page with an earlier entry behind it, so a
 * back step has somewhere to land. */
function renderWithHistory(route: string) {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={["/elsewhere", route]} initialIndex={1}>
      <QueryClientProvider client={queryClient}>
        <LocationSpy />
        <BackControl />
        <Routes>
          <Route path="/elsewhere" element={<div>Somewhere else</div>} />
          <Route path="/pipelines/:tenant" element={<TenantHome />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

/** pipelineList builds a listing response for the tenant folder named. */
function pipelineList(
  tenantFolder: string,
  jobPaths: Array<string | { jobPath: string; kind?: "pipeline" | "multibranch" }>,
  runningPaths: string[] = [],
) {
  return {
    items: jobPaths.map((entry) =>
      typeof entry === "string"
        ? createPipelineSummary({ jobPath: entry })
        : createPipelineSummary({ jobPath: entry.jobPath, kind: entry.kind ?? "pipeline" }),
    ),
    runningNow: runningPaths.map((jobPath) => createRunningRun({ jobPath })),
    tenantFolder,
    truncated: false,
  };
}

describe("TenantHome", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("shows a loading state before the response arrives", () => {
    mockListPipelines.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByText(/Loading pipelines/)).toBeInTheDocument();
  });

  it("shows the effective retention window", async () => {
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [],
      truncated: false,
      retention: { storeDays: 365, tenantDays: 30, effectiveDays: 30, jobs: [] },
    });
    renderPage();
    await waitFor(() =>
      expect(screen.getByTestId("tenant-retention")).toHaveTextContent(
        "Build history kept 30 days (store limit 365 days)",
      ),
    );
  });

  it("lists job retention rules", async () => {
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [],
      truncated: false,
      retention: {
        storeDays: 365,
        effectiveDays: 365,
        jobs: [{ job: "pr-*", days: 7, effectiveDays: 7 }],
      },
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("pr-*: 7 days")).toBeInTheDocument());
  });

  it("sets the document title from the tenant", async () => {
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    renderPage();
    await waitFor(() => expect(document.title).toBe("acme · Pipelines"));
  });

  it("shows the empty state for a tenant with no running builds and no pipelines", async () => {
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Nothing is running.")).toBeInTheDocument();
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
  });

  it("renders a never-run job with an empty last build and no health glyph", async () => {
    mockListPipelines.mockResolvedValue({
      items: [{ jobPath: "payments/checkout-api/main", health: { bucket: "unknown", window: 10, counted: 0, passed: 0 } }],
      runningNow: [],
      truncated: false,
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /^#/ })).not.toBeInTheDocument();
  });

  it("names the health state for a screen reader", async () => {
    mockListPipelines.mockResolvedValue({
      items: [{ jobPath: "payments/checkout-api/main", health: { bucket: "rainy", window: 10, counted: 3, passed: 0 } }],
      runningNow: [],
      truncated: false,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("img", { name: /Rainy/ })).toBeInTheDocument();
    });
  });

  it("states that the running-now view is bounded when the API reports truncation", async () => {
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [
        { runId: "r1", jobPath: "team/a/main", status: "RUNNING", buildNumber: 12, currentStage: "Build" },
      ],
      truncated: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/bounded/)).toBeInTheDocument();
    });
  });

  it("names the stage in progress on a running tile, and Starting or Queued without one", async () => {
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [
        {
          runId: "r1", jobPath: "team/a/main", status: "RUNNING", buildNumber: 12,
          currentStage: "Build",
        },
        {
          runId: "r2", jobPath: "team/b/main", status: "RUNNING", buildNumber: 13,
          startedAt: "2026-01-01T00:00:00Z",
        },
        { runId: "r3", jobPath: "team/c/main", status: "QUEUED", buildNumber: 14 },
      ],
      truncated: false,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Build")).toBeInTheDocument();
    });
    expect(screen.getByText("Starting")).toBeInTheDocument();
    expect(screen.getByText("Queued")).toBeInTheDocument();
  });

  it("renders the forbidden state on a 403", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(403, "no"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Access denied")).toBeInTheDocument();
    });
  });

  it("renders the generic error state on a 500", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(500, "boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load page")).toBeInTheDocument();
    });
  });

  it("explains a disabled record store on a 503", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(503, "off"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Pipeline data is off")).toBeInTheDocument();
    });
  });

  it("explains an unreachable record store on a 503 with the unreachable code", async () => {
    mockListPipelines.mockRejectedValue(
      new ApiError(503, "record store unreachable", { error: "record store unreachable", code: "record_store_unreachable" }),
    );
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Pipeline data is unreachable")).toBeInTheDocument();
    });
    expect(screen.queryByText("Pipeline data is off")).not.toBeInTheDocument();
  });
});

describe("TenantHome — the folder tree", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("turns a nested job path into a folder row and a job row", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/release/eu"]));
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "release, 1 job" })).toBeInTheDocument();
    });
    expect(screen.getByRole("link", { name: "eu" })).toBeInTheDocument();
  });

  it("renders no row for an item whose path equals the tenant folder", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments"]));
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
    expect(screen.queryByRole("link", { name: "payments" })).not.toBeInTheDocument();
  });

  it("removes a folded folder's rows from the page, rather than hiding them", async () => {
    const user = userEvent.setup();
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/release/eu"]));
    renderPage("/pipelines/payments");
    await waitFor(() => expect(screen.getByRole("link", { name: "eu" })).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "release, 1 job" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "eu" })).not.toBeInTheDocument());
    expect(screen.queryByText("payments/release/eu")).not.toBeInTheDocument();
  });

  it("puts a folder row before a job row at the same level", async () => {
    mockListPipelines.mockResolvedValue(
      pipelineList("payments", ["payments/zeta", "payments/api", "payments/release/eu"]),
    );
    renderPage("/pipelines/payments");
    await waitFor(() => expect(screen.getByRole("link", { name: "api" })).toBeInTheDocument());

    const cells = screen.getAllByRole("cell").filter((cell) => cell.textContent !== "");
    const names = cells.map((cell) => cell.textContent);
    const releaseAt = names.findIndex((text) => text?.startsWith("release"));
    const apiAt = names.findIndex((text) => text?.startsWith("api"));
    const zetaAt = names.findIndex((text) => text?.startsWith("zeta"));
    expect(releaseAt).toBeLessThan(apiAt);
    expect(apiAt).toBeLessThan(zetaAt);
  });

  it("reports a folder's job count and running count, and renders no health glyph", async () => {
    mockListPipelines.mockResolvedValue(
      pipelineList(
        "payments",
        ["payments/release/eu", "payments/release/us"],
        ["payments/release/eu", "payments/release/us"],
      ),
    );
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "release, 2 jobs" })).toBeInTheDocument();
    });
    expect(screen.getByText("2 running")).toBeInTheDocument();
    expect(screen.queryByRole("img", { name: /Clear|Cloudy|Rain/ })).not.toBeInTheDocument();
  });

  it("renders a multibranch item as a foldable row naming its branches", async () => {
    mockListPipelines.mockResolvedValue(
      pipelineList("payments", [
        { jobPath: "payments/checkout-api", kind: "multibranch" },
        { jobPath: "payments/checkout-api/main" },
        { jobPath: "payments/checkout-api/dev" },
      ]),
    );
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "checkout-api, 2 branches" })).toBeInTheDocument();
    });
    expect(screen.getByText("2 branches")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Run/ })).not.toBeInTheDocument();
  });
});

describe("TenantHome — the fold state", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("writes the unfolded folders into the URL", async () => {
    const user = userEvent.setup();
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/release/eu/deploy"]));
    renderPage("/pipelines/payments?open=release");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "release/eu, 1 job" })).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "release/eu, 1 job" }));
    await waitFor(() => {
      const search = screen.getByTestId("search").textContent ?? "";
      expect(search).toContain("open=release");
      expect(search).toContain("open=release%2Feu");
    });
  });

  it("unfolds the top two levels on a first load, and folds a third level", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/a/b/c/job"]));
    renderPage("/pipelines/payments");
    await waitFor(() => expect(screen.getByRole("button", { name: "a, 1 job" })).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "a/b, 1 job" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "a/b/c, 1 job" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "job" })).not.toBeInTheDocument();
  });

  it("folds everything when the open parameter is present and empty", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/release/eu"]));
    renderPage("/pipelines/payments?open=");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "release, 1 job" })).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: "release, 1 job" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
    expect(screen.queryByRole("link", { name: "eu" })).not.toBeInTheDocument();
  });

  it("leaves the page on a back step, rather than restoring an earlier fold", async () => {
    const user = userEvent.setup();
    mockListPipelines.mockResolvedValue(
      pipelineList("payments", ["payments/a/x", "payments/b/x", "payments/c/x"]),
    );
    renderWithHistory("/pipelines/payments");
    await waitFor(() => expect(screen.getByRole("button", { name: "a, 1 job" })).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: "a, 1 job" }));
    await user.click(screen.getByRole("button", { name: "b, 1 job" }));
    await user.click(screen.getByRole("button", { name: "c, 1 job" }));

    await user.click(screen.getByRole("button", { name: "Step back" }));
    await waitFor(() => expect(screen.getByText("Somewhere else")).toBeInTheDocument());
  });

  it("folds and unfolds the focused node with the arrow keys", async () => {
    const user = userEvent.setup();
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/release/eu"]));
    renderPage("/pipelines/payments?open=");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "release, 1 job" })).toBeInTheDocument();
    });

    screen.getByRole("button", { name: "release, 1 job" }).focus();
    await user.keyboard("{ArrowRight}");
    await waitFor(() => expect(screen.getByRole("link", { name: "eu" })).toBeInTheDocument());

    screen.getByRole("button", { name: "release, 1 job" }).focus();
    await user.keyboard("{ArrowLeft}");
    await waitFor(() => expect(screen.queryByRole("link", { name: "eu" })).not.toBeInTheDocument());
  });

  it("changes the rows at once and moves no indicator for a reader who refuses motion", async () => {
    const user = userEvent.setup();
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/release/eu"]));
    renderPage("/pipelines/payments");
    await waitFor(() => expect(screen.getByRole("link", { name: "eu" })).toBeInTheDocument());

    // The row set changes with the click, with no height animation between.
    await user.click(screen.getByRole("button", { name: "release, 1 job" }));
    expect(screen.queryByRole("link", { name: "eu" })).not.toBeInTheDocument();

    const css = fs.readFileSync(path.join(__dirname, "JobTree.module.css"), "utf-8");
    expect(css).toMatch(/@media \(prefers-reduced-motion: reduce\)[\s\S]*transition: none/);
  });

  it("tabs over the disclosure buttons in row order", async () => {
    mockListPipelines.mockResolvedValue(
      pipelineList("payments", ["payments/a/x", "payments/b/x"]),
    );
    renderPage("/pipelines/payments");
    await waitFor(() => expect(screen.getByRole("button", { name: "a, 1 job" })).toBeInTheDocument());

    const labels = screen.getAllByRole("button").map((button) => button.getAttribute("aria-label"));
    expect(labels).toEqual(["a, 1 job", "b, 1 job"]);
  });

  it("states that the list shows only the jobs the caller's access covers", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", []));
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByText("This list shows only the jobs your access covers.")).toBeInTheDocument();
    });
  });
});

describe("TenantHome — Runners section", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockListFleetLocks.mockReset();
    mockListFleetLocks.mockResolvedValue({ items: [] });
    mockGetTenantCell.mockReset();
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("shows the recovery state for a tenant with no registered runners", async () => {
    mockListRunners.mockResolvedValue([]);
    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText("No runners. An operator registers a Runner for this tenant."),
      ).toBeInTheDocument();
    });
  });

  it("renders a runner row with its launcher, labels, cell, and connection state", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "win-1",
        spec: { tenant: "acme", launcher: "ssh", labels: ["windows", "msbuild"], remoteFS: "C:\\J" },
        status: { nodeName: "acme-win-1", cell: "acme-cell-1", connected: true, lastSeen: new Date().toISOString() },
      },
    ]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("win-1")).toBeInTheDocument();
    });
    expect(screen.getByText("ssh")).toBeInTheDocument();
    expect(screen.getByText("windows, msbuild")).toBeInTheDocument();
    expect(screen.getByText("acme-cell-1")).toBeInTheDocument();
    expect(screen.getByText("Connected", { selector: "span" })).toBeInTheDocument();
  });

  it("puts the pipelines, locks, and runners tables in a horizontal scroll container", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("acme", ["acme/release/eu"]));
    mockListFleetLocks.mockResolvedValue({ items: [createFleetLock({ name: "acme/smoke-lock" })] });
    mockListRunners.mockResolvedValue([
      {
        name: "win-1",
        spec: { tenant: "acme", launcher: "ssh", labels: [], remoteFS: "C:\\J" },
        status: { nodeName: "acme-win-1", cell: "acme-cell-1", connected: true, lastSeen: new Date().toISOString() },
      },
    ]);
    renderPage();
    await waitFor(() => {
      expect(document.querySelectorAll("table")).toHaveLength(3);
    });
    for (const table of Array.from(document.querySelectorAll("table"))) {
      expect(table.parentElement?.className).toMatch(/tableScroll/);
    }
  });

  it("renders an unconnected runner with dashes for its unset fields", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "agent-1",
        spec: { tenant: "acme", launcher: "inbound", remoteFS: "/home/jenkins" },
        status: { connected: false },
      },
    ]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("agent-1")).toBeInTheDocument();
    });
    expect(screen.getByText("inbound")).toBeInTheDocument();
    expect(screen.getByText("Not connected")).toBeInTheDocument();
  });

  it("never renders status.token, even when the API response carries one", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "agent-1",
        spec: { tenant: "acme", launcher: "inbound", remoteFS: "/home/jenkins" },
        status: { connected: true, token: "do-not-render-me" },
      },
    ]);
    const { container } = renderPage();
    await waitFor(() => {
      expect(screen.getByText("agent-1")).toBeInTheDocument();
    });
    expect(container.textContent).not.toContain("do-not-render-me");
  });

  it("shows a no-runners-loaded message when the runners request fails", async () => {
    mockListRunners.mockRejectedValue(new Error("boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load runners.")).toBeInTheDocument();
    });
  });
});

describe("TenantHome — the cell card", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/checkout-api"]));
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("puts the pill in the page head after the title and opens the card from it", async () => {
    const user = userEvent.setup();
    mockGetTenantCell.mockResolvedValue({
      state: "placed",
      reason: "TenantPlaced",
      cell: "cell-a",
      namespace: "varroa-system",
      cluster: "core",
      freeSlots: 4,
      queueLength: 0,
      runningNow: 1,
      otherTenantCount: 2,
      canOpenCell: false,
    });
    renderPage("/pipelines/payments");

    const pill = await screen.findByRole("button", { name: "Placed · cell-a" });
    const title = screen.getByRole("heading", { level: 1, name: "payments" });
    const running = screen.getByText("Running now");
    expect(title.compareDocumentPosition(pill) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    // A closed disclosure keeps the card out of the accessibility tree, so
    // the region arrives only once a reader selects the pill.
    expect(screen.queryByRole("region", { name: "Your cell" })).not.toBeInTheDocument();

    await user.click(pill);
    const card = screen.getByRole("region", { name: "Your cell" });
    expect(pill.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(card.compareDocumentPosition(running) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows the member cluster, tenant count, and free slots beside the pill", async () => {
    mockListClusters.mockResolvedValue([
      { name: "core", core: true },
      { name: "member-a", core: false },
    ]);
    mockGetTenantCell.mockResolvedValue({
      state: "placed",
      cell: "cell-a",
      cluster: "member-a",
      namespace: "varroa-system",
      freeSlots: 4,
      otherTenantCount: 2,
      canOpenCell: false,
    });
    renderPage("/pipelines/payments");

    await screen.findByRole("button", { name: "Placed · cell-a" });
    const meta = await screen.findByTestId("tenant-cell-meta");
    await waitFor(() => expect(within(meta).getByText("member cluster member-a")).toBeInTheDocument());
    expect(meta).toHaveTextContent("2 other tenants");
    expect(meta).toHaveTextContent("4 free slots");
  });

  it("shows no member chip for a core-placed tenant", async () => {
    mockGetTenantCell.mockResolvedValue({
      state: "placed",
      cell: "cell-a",
      cluster: "core",
      freeSlots: 1,
      canOpenCell: false,
    });
    renderPage("/pipelines/payments");

    const meta = await screen.findByTestId("tenant-cell-meta");
    expect(within(meta).queryByText(/member cluster/)).not.toBeInTheDocument();
    expect(meta).toHaveTextContent("1 free slot");
  });

  it("links Open cell only when the caller may open the cell", async () => {
    mockGetTenantCell.mockResolvedValue({
      state: "placed",
      cell: "cell-a",
      cluster: "core",
      namespace: "varroa-system",
      canOpenCell: true,
    });
    renderPage("/pipelines/payments");

    const link = await screen.findByRole("link", { name: "Open cell" });
    expect(link.getAttribute("href")).toBe("/controllers/core/varroa-system/cell-a?tab=cell");
  });

  it("omits Open cell when the caller may not open the cell", async () => {
    mockGetTenantCell.mockResolvedValue({
      state: "placed",
      cell: "cell-a",
      cluster: "core",
      namespace: "varroa-system",
      freeSlots: 2,
      canOpenCell: false,
    });
    renderPage("/pipelines/payments");

    await screen.findByTestId("tenant-cell-meta");
    expect(screen.queryByRole("link", { name: "Open cell" })).not.toBeInTheDocument();
  });

  it("renders empty Running now, Locks, and Runners as one-line notes", async () => {
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    renderPage("/pipelines/payments");

    expect(await screen.findByText("Nothing is running.")).toBeInTheDocument();
    expect(await screen.findByText("The tenant holds no lock and waits on none.")).toBeInTheDocument();
    expect(
      await screen.findByText("No runners. An operator registers a Runner for this tenant."),
    ).toBeInTheDocument();
  });

  it("shows the warning message beside the pill", async () => {
    mockGetTenantCell.mockResolvedValue({
      state: "placed",
      cell: "cell-a",
      cluster: "core",
      freeSlots: 0,
      canOpenCell: false,
      warning: { reason: "TenantCapReached", message: "The cell is at its tenant cap." },
    });
    renderPage("/pipelines/payments");

    const meta = await screen.findByTestId("tenant-cell-meta");
    expect(within(meta).getByText("The cell is at its tenant cap.")).toBeInTheDocument();
  });

  it("heads the page with an h1 and the card with an h2", async () => {
    const user = userEvent.setup();
    mockGetTenantCell.mockResolvedValue({ state: "placed", cell: "cell-a", canOpenCell: false });
    renderPage("/pipelines/payments");

    await user.click(await screen.findByRole("button", { name: "Placed · cell-a" }));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("payments");
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Your cell");
  });

  it("keeps the pipelines table when the cell read fails", async () => {
    mockGetTenantCell.mockRejectedValue(new Error("boom"));
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    await waitFor(() => expect(screen.queryByText("Your cell")).not.toBeInTheDocument());
  });

  it("refreshes the cell read on its own interval and leaves the other reads alone", async () => {
    mockGetTenantCell.mockClear();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderPage("/pipelines/payments");
      await waitFor(() => expect(mockGetTenantCell).toHaveBeenCalledTimes(1));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });

      expect(mockGetTenantCell).toHaveBeenCalledTimes(3);
      expect(mockListPipelines).toHaveBeenCalledTimes(1);
      expect(mockListRunners).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("TenantHome — the cell pill", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/checkout-api"]));
  });

  /** cellPill finds the pill by the region it controls, so a page that
   * renders none answers null. */
  function cellPill(): Element | null {
    return document.querySelector('[aria-controls="tenant-cell-disclosure"]');
  }

  it("renders each cell-hosting state's pill with its own label, and a cell name only when the API sends one", async () => {
    const rows = [
      ["placed", "Placed · cell-a", "cell-a"],
      ["moving", "Moving · cell-a", "cell-a"],
      ["rehydrating", "Rehydrating · cell-a", "cell-a"],
      ["unplaced", "Not placed", undefined],
    ] as const;

    for (const [state, name, cell] of rows) {
      // The API omits `cell` for a state it has not placed, so the fixture
      // omits the key rather than sending a name the pill would show.
      mockGetTenantCell.mockResolvedValue(
        cell ? { state, cell, canOpenCell: false } : { state, canOpenCell: false },
      );
      const { unmount } = renderPage("/pipelines/payments");

      const pill = await screen.findByRole("button", { name });
      expect(pill.textContent).toBe(name);
      unmount();
    }
  });

  it("renders no pill for a tenant on its own controller", async () => {
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    expect(cellPill()).toBeNull();
  });

  it("renders no pill while the cell read is pending", async () => {
    mockGetTenantCell.mockReturnValue(new Promise(() => {}));
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    expect(cellPill()).toBeNull();
  });

  it("renders no pill when the first cell read fails", async () => {
    mockGetTenantCell.mockRejectedValue(new Error("boom"));
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    expect(cellPill()).toBeNull();
  });

  it("hides the pill when a successful read is followed by a failed refetch", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetTenantCell.mockResolvedValueOnce({ state: "placed", cell: "cell-a", canOpenCell: false });
      mockGetTenantCell.mockRejectedValue(new Error("boom"));
      renderPage("/pipelines/payments");

      expect(await screen.findByRole("button", { name: "Placed · cell-a" })).toBeInTheDocument();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });

      await waitFor(() => expect(cellPill()).toBeNull());
    } finally {
      vi.useRealTimers();
    }
  });

  it("toggles aria-expanded and reveals the disclosure", async () => {
    const user = userEvent.setup();
    mockGetTenantCell.mockResolvedValue({ state: "placed", cell: "cell-a", canOpenCell: false });
    renderPage("/pipelines/payments");

    const pill = await screen.findByRole("button", { name: "Placed · cell-a" });
    expect(pill).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("region", { name: "Your cell" })).not.toBeInTheDocument();

    await user.click(pill);
    await waitFor(() => expect(pill).toHaveAttribute("aria-expanded", "true"));
    expect(screen.getByRole("region", { name: "Your cell" })).toBeInTheDocument();

    await user.click(pill);
    await waitFor(() => expect(pill).toHaveAttribute("aria-expanded", "false"));
    expect(screen.queryByRole("region", { name: "Your cell" })).not.toBeInTheDocument();
  });

  it("renders a Queue wait value, and an em dash for a job with none", async () => {
    mockListPipelines.mockResolvedValue({
      items: [
        createPipelineSummary({ jobPath: "payments/with-wait", medianQueueWaitMs: 30000 }),
        createPipelineSummary({ jobPath: "payments/without-wait" }),
      ],
      runningNow: [],
      tenantFolder: "payments",
      truncated: false,
    });
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("Queue wait")).toBeInTheDocument());
    const withWait = screen.getByRole("link", { name: "with-wait" }).closest("tr") as HTMLTableRowElement;
    const withoutWait = screen.getByRole("link", { name: "without-wait" }).closest("tr") as HTMLTableRowElement;

    expect(withWait.querySelectorAll("td")[5].textContent).toBe("30s");
    expect(withoutWait.querySelectorAll("td")[5].textContent).toBe("—");
  });
});

describe("TenantHome — live run events", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/checkout-api"]));
  });

  it("subscribes to the tenant's own stream with every event name", async () => {
    renderPage("/pipelines/payments");
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(1));

    // The URL carries the BFF base, so the browser reaches the API and not
    // the SPA fallback. The stream opens once the main read has succeeded.
    await waitFor(() =>
      expect(mockUseEventStream).toHaveBeenCalledWith(
        "/api/v1/pipelines/payments/stream",
        "pipelines:payments",
        ["run", "stage", "lock"],
      ),
    );
    expect(mockUseEventStream).toHaveBeenCalledWith(
      "/api/v1/pipelines/payments/stream",
      "pipelines:payments",
      ["run", "stage", "lock"],
    );
  });

  it("refetches the tenant read for a run event", async () => {
    const { repaint } = renderPage("/pipelines/payments");
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(1));

    mockUseEventStream.mockReturnValue({
      lastEvent: { type: "run", data: notification() },
      readyState: "closed",
      error: null,
    });
    await act(async () => {
      repaint();
    });

    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(2));
  });

  it("reads once per window, and reads again when a frame inside the window ends it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { repaint } = renderPage("/pipelines/payments");
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(1));

      // A run frame outside the window reads at once.
      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "run", data: notification() },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(2));
      expect(mockListRunners).toHaveBeenCalledTimes(1);

      // A stage frame 500ms later arms one read for the end of the window.
      await act(async () => {
        vi.advanceTimersByTime(500);
      });
      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "stage", data: notification({ kind: "stage", seq: 2 }) },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });
      await act(async () => {
        await Promise.resolve();
      });
      expect(mockListPipelines).toHaveBeenCalledTimes(2);

      // A second stage frame inside the same window arms no second read.
      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "stage", data: notification({ kind: "stage", seq: 3 }) },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });
      await act(async () => {
        await Promise.resolve();
      });
      expect(mockListPipelines).toHaveBeenCalledTimes(2);

      // The window ends 2 seconds after the run frame, and one read lands.
      await act(async () => {
        vi.advanceTimersByTime(2000);
      });
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(3));
      expect(mockListRunners).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("reads once when a frame outside the window finds a pending timer", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { repaint } = renderPage("/pipelines/payments");
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(1));

      // A run frame reads at once and stamps the window.
      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "run", data: notification() },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(2));

      // A stage frame 500ms later arms one read for the end of the window.
      await act(async () => {
        vi.advanceTimersByTime(500);
      });
      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "stage", data: notification({ kind: "stage", seq: 2 }) },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });

      // A busy loop can hold that read past the window. The clock then
      // stands beyond the window end while the timer has not run yet.
      await act(async () => {
        vi.setSystemTime(Date.now() + 2500);
      });
      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "stage", data: notification({ kind: "stage", seq: 3 }) },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(3));

      // The stale timer reads nothing, so the window holds one read.
      await act(async () => {
        vi.advanceTimersByTime(2000);
      });
      expect(mockListPipelines).toHaveBeenCalledTimes(3);
      expect(mockListRunners).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("refetches the tenant read on a reconnect, before it applies a later event", async () => {
    const { repaint } = renderPage("/pipelines/payments");
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(1));

    // The stream comes back open with no frame yet: that alone is the
    // page's signal to refetch.
    mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "open", error: null });
    await act(async () => {
      repaint();
    });
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(2));

    // A frame of the reconnected stream arrives after the open read.
    mockUseEventStream.mockReturnValue({
      lastEvent: { type: "run", data: notification({ seq: 2 }) },
      readyState: "open",
      error: null,
    });
    await act(async () => {
      repaint();
    });
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(3));
  });

  it("runs no cell read for a run event, and leaves the card on its own interval", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetTenantCell.mockResolvedValue({ state: "placed", cell: "cell-a", canOpenCell: false });
      const { repaint } = renderPage("/pipelines/payments");
      await waitFor(() => expect(mockGetTenantCell).toHaveBeenCalledTimes(1));

      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "run", data: notification() },
        readyState: "closed",
        error: null,
      });
      await act(async () => {
        repaint();
      });
      await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(2));
      expect(mockGetTenantCell).toHaveBeenCalledTimes(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });

      expect(mockGetTenantCell).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("refetches the new tenant's read for a run event inside the old tenant's window", async () => {
    const user = userEvent.setup();
    const queryClient = createTestQueryClient();
    // The switch keeps one TenantHome instance across two tenants, which is what
    // a reader does with the navigation.
    function TenantSwitch() {
      const navigate = useNavigate();
      return (
        <button type="button" onClick={() => navigate("/pipelines/payments")}>
          Switch tenant
        </button>
      );
    }
    const tree = () => (
      <MemoryRouter initialEntries={["/pipelines/acme"]}>
        <QueryClientProvider client={queryClient}>
          <Routes>
            <Route path="/pipelines/:tenant" element={<TenantHome />} />
          </Routes>
          <TenantSwitch />
        </QueryClientProvider>
      </MemoryRouter>
    );

    const view = render(tree());
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(1));

    // A run event refetches the first tenant's read.
    mockUseEventStream.mockReturnValue({
      lastEvent: { type: "run", data: notification({ tenant: "acme" }) },
      readyState: "closed",
      error: null,
    });
    await act(async () => {
      view.rerender(tree());
    });
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledTimes(2));

    // The reader switches tenant, and the switch costs the new tenant one read.
    await user.click(screen.getByRole("button", { name: "Switch tenant" }));
    await waitFor(() => expect(mockListPipelines.mock.calls.length).toBeGreaterThanOrEqual(3));
    await act(async () => {
      await Promise.resolve();
    });
    const settled = mockListPipelines.mock.calls.length;

    // The new tenant's first frame arrives inside the old window, and the new
    // tenant starts a new window.
    mockUseEventStream.mockReturnValue({
      lastEvent: { type: "run", data: notification({ tenant: "payments", seq: 2 }) },
      readyState: "closed",
      error: null,
    });
    await act(async () => {
      view.rerender(tree());
    });

    await waitFor(() => expect(mockListPipelines.mock.calls.length).toBeGreaterThan(settled));
  });
});

describe("TenantHome — a tenant the main read does not find", () => {
  const waitingTenant = {
    name: "acme",
    namespaces: [],
    pipelines: { profile: "p", agentNamespace: "n" },
  };

  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListFleetLocks.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockListFleetLocks.mockResolvedValue({ items: [] });
    mockGetTenantCell.mockResolvedValue({ state: "unplaced", reason: "ProfileNotFound", canOpenCell: false });
    mockListPipelines.mockRejectedValue(new ApiError(404, "tenant not found"));
  });

  it("shows the Not placed pill and the waiting note for a pipelines Tenant with no cell", async () => {
    mockGetTenant.mockResolvedValue(waitingTenant);
    const user = userEvent.setup();
    renderPage("/pipelines/acme");

    expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("acme");
    const pill = await screen.findByRole("button", { name: "Not placed" });
    expect(screen.getByText("This tenant has no cell yet.")).toBeInTheDocument();
    expect(screen.queryByText("Not found")).not.toBeInTheDocument();

    await user.click(pill);
    expect(screen.getByText("ProfileNotFound")).toBeInTheDocument();
  });

  it("sends no runners, locks, or stream request for a waiting tenant", async () => {
    mockGetTenant.mockResolvedValue(waitingTenant);
    renderPage("/pipelines/acme");

    await screen.findByRole("button", { name: "Not placed" });
    expect(mockListRunners).not.toHaveBeenCalled();
    expect(mockListFleetLocks).not.toHaveBeenCalled();
    const last = mockUseEventStream.mock.calls[mockUseEventStream.mock.calls.length - 1];
    expect(last?.[0]).toBeNull();
    expect(last?.[1]).toBeNull();
  });

  it("shows the no-pipelines page for a readable Tenant with no pipelines", async () => {
    mockGetTenant.mockResolvedValue({ name: "acme", namespaces: [] });
    renderPage("/pipelines/acme");

    expect(await screen.findByText("No pipelines")).toBeInTheDocument();
    expect(screen.queryByText("Not found")).not.toBeInTheDocument();
  });

  it("shows Not found when the Tenant read is refused", async () => {
    mockGetTenant.mockRejectedValue(new ApiError(403, "forbidden"));
    renderPage("/pipelines/acme");

    expect(await screen.findByText("Not found")).toBeInTheDocument();
  });

  it("shows Not found when no Tenant exists", async () => {
    mockGetTenant.mockRejectedValue(new ApiError(404, "tenant not found"));
    renderPage("/pipelines/acme");

    expect(await screen.findByText("Not found")).toBeInTheDocument();
  });

  it("issues no dependent request while the main read is pending", async () => {
    mockListPipelines.mockReturnValue(new Promise(() => {}));
    renderPage("/pipelines/acme");

    await screen.findByText(/Loading pipelines/);
    expect(mockListRunners).not.toHaveBeenCalled();
    expect(mockListFleetLocks).not.toHaveBeenCalled();
    expect(mockGetTenantCell).not.toHaveBeenCalled();
    expect(mockGetTenant).not.toHaveBeenCalled();
    expect(mockUseEventStream.mock.calls[mockUseEventStream.mock.calls.length - 1]?.[0]).toBeNull();
  });

  it("issues no dependent request and no Tenant read when the main read fails with a server error", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(500, "boom"));
    renderPage("/pipelines/acme");

    expect(await screen.findByText("Unable to load page")).toBeInTheDocument();
    expect(mockListRunners).not.toHaveBeenCalled();
    expect(mockListFleetLocks).not.toHaveBeenCalled();
    expect(mockGetTenantCell).not.toHaveBeenCalled();
    expect(mockGetTenant).not.toHaveBeenCalled();
    expect(mockUseEventStream.mock.calls[mockUseEventStream.mock.calls.length - 1]?.[0]).toBeNull();
  });
});

describe("TenantHome — a waiting tenant's transitions", () => {
  const waitingTenant = {
    name: "acme",
    namespaces: [],
    pipelines: { profile: "p", agentNamespace: "n" },
  };

  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListFleetLocks.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockListFleetLocks.mockResolvedValue({ items: [] });
    mockGetTenantCell.mockResolvedValue({ state: "unplaced", reason: "ProfileNotFound", canOpenCell: false });
    mockListPipelines.mockRejectedValue(new ApiError(404, "tenant not found"));
  });

  it("renders the tenant page and opens the stream after the waiting tenant is placed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetTenant.mockResolvedValue(waitingTenant);
      renderPage("/pipelines/acme");
      await screen.findByRole("button", { name: "Not placed" });
      expect(screen.getByText("This tenant has no cell yet.")).toBeInTheDocument();

      // The operator places the Tenant. The main read now succeeds on its
      // 30 second poll.
      mockListPipelines.mockResolvedValue(pipelineList("acme", ["acme/checkout-api"]));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });

      await waitFor(() => expect(screen.queryByText("This tenant has no cell yet.")).not.toBeInTheDocument());
      expect(screen.getByText("Running now")).toBeInTheDocument();
      expect(screen.getByText("Locks")).toBeInTheDocument();
      expect(screen.getByText("Runners")).toBeInTheDocument();
      expect(mockListRunners).toHaveBeenCalled();
      expect(mockListFleetLocks).toHaveBeenCalled();
      const last = mockUseEventStream.mock.calls[mockUseEventStream.mock.calls.length - 1];
      expect(last?.[0]).toBe("/api/v1/pipelines/acme/stream");
      expect(last?.[1]).toBe("pipelines:acme");
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling when the Tenant has no pipelines", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetTenant.mockResolvedValue({ name: "acme", namespaces: [] });
      renderPage("/pipelines/acme");
      await screen.findByText("No pipelines");

      const before = mockListPipelines.mock.calls.length;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      expect(mockListPipelines).toHaveBeenCalledTimes(before);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("TenantHome — the proposal card", () => {
  const writer = {
    global: {},
    scopes: [],
    tenants: [{ tenant: "payments", jobs: [], capabilities: { pipelines: { read: true, write: true } } }],
  };
  const reader = {
    global: {},
    scopes: [],
    tenants: [{ tenant: "payments", jobs: [], capabilities: { pipelines: { read: true } } }],
  };

  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTenantCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    mockAssistant.available = true;
    mockAssistant.switchOn = true;
  });

  it("shows the card for a writer on an empty placed tenant", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", []));
    mockGetMyPermissions.mockResolvedValue(writer);
    renderPage("/pipelines/payments");
    expect(await screen.findByText("propose-card")).toBeInTheDocument();
    expect(screen.getByText("This list shows only the jobs your access covers.")).toBeInTheDocument();
  });

  it("hides the card from a reader", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", []));
    mockGetMyPermissions.mockResolvedValue(reader);
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
    expect(screen.queryByText("propose-card")).not.toBeInTheDocument();
  });

  it("hides the card for a synthetic tenant", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("", []));
    mockGetMyPermissions.mockResolvedValue(writer);
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
    expect(screen.queryByText("propose-card")).not.toBeInTheDocument();
  });

  it("hides the card when the assistant is unavailable", async () => {
    mockAssistant.available = false;
    mockListPipelines.mockResolvedValue(pipelineList("payments", []));
    mockGetMyPermissions.mockResolvedValue(writer);
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
    expect(screen.queryByText("propose-card")).not.toBeInTheDocument();
  });

  it("hides the card when the tenant already has jobs", async () => {
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/deploy"]));
    mockGetMyPermissions.mockResolvedValue(writer);
    renderPage("/pipelines/payments");
    await waitFor(() => {
      expect(document.querySelector("table")).not.toBeNull();
    });
    expect(screen.queryByText("propose-card")).not.toBeInTheDocument();
  });
});
