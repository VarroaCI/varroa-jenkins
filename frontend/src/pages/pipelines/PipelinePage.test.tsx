import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import type { RunNotification } from "../../types";
import PipelinePage from "./PipelinePage";

const mockGetPipelineJob = vi.fn();
const mockListRuns = vi.fn();
const mockCreateRun = vi.fn();
vi.mock("../../api/client", () => ({
  getPipelineJob: (...args: unknown[]) => mockGetPipelineJob(...args),
  listRuns: (...args: unknown[]) => mockListRuns(...args),
  createRun: (...args: unknown[]) => mockCreateRun(...args),
}));

/** RunEvent holds one frame the page reads off the team stream. */
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

vi.mock("../../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../../hooks/usePermissions")>(
    "../../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn(() => ({ data: undefined })) };
});
import { usePermissions } from "../../hooks/usePermissions";

/** notification builds one stream frame of the job under test. */
function notification(overrides: Partial<RunNotification> = {}): RunNotification {
  return {
    kind: "run",
    cluster: "core",
    tenant: "acme",
    job_path: "payments/checkout-api/main",
    run_id: "run-1",
    seq: 1,
    at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  mockUseEventStream.mockClear();
  mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
});

function LocationProbe() {
  const location = useLocation();
  return (
    <>
      <div data-testid="location">
        {location.pathname}
        {location.search}
      </div>
      <div data-testid="location-state">{JSON.stringify(location.state ?? null)}</div>
    </>
  );
}

/** renderPage returns repaint, which re-renders the page so it reads the
 * stream state the test just set. */
function renderPage(route = "/pipelines/acme/payments%2Fcheckout-api%2Fmain") {
  const queryClient = createTestQueryClient();
  const tree = () => (
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines/:team/:job" element={<PipelinePage />} />
          {/* Matches the run URL PipelinePage navigates to on submit, so
              that navigation resolves instead of falling through to no
              route. */}
          <Route path="/pipelines/:team/:job/:run" element={<div data-testid="run-page" />} />
        </Routes>
        <LocationProbe />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree());
  return { ...view, repaint: () => view.rerender(tree()) };
}

const jobDetail = (overrides: Record<string, unknown> = {}) => ({
  jobPath: "payments/checkout-api/main",
  tenantFolder: "",
  tenant: "acme",
  health: "clear",
  last30Days: null,
  dependencies: { upstream: [], downstream: [] },
  ...overrides,
});

describe("PipelinePage", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockListRuns.mockResolvedValue({ items: [] });
    mockCreateRun.mockReset();
    vi.mocked(usePermissions).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof usePermissions>);
  });

  it("shows a loading state before the job response arrives", () => {
    mockGetPipelineJob.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByText(/Loading pipeline/)).toBeInTheDocument();
  });

  it("shows the empty builds state", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("No builds match this filter.")).toBeInTheDocument());
  });

  it("renders the forbidden state on a 403", async () => {
    mockGetPipelineJob.mockRejectedValue(new ApiError(403, "no"));
    renderPage();
    await waitFor(() => expect(screen.getByText("Access denied")).toBeInTheDocument());
  });

  it("renders the not-found state on a 404", async () => {
    mockGetPipelineJob.mockRejectedValue(new ApiError(404, "no"));
    renderPage();
    await waitFor(() => expect(screen.getByText("Not found")).toBeInTheDocument());
  });

  it("renders the generic error state on a 500", async () => {
    mockGetPipelineJob.mockRejectedValue(new ApiError(500, "boom"));
    renderPage();
    await waitFor(() => expect(screen.getByText("Unable to load page")).toBeInTheDocument());
  });

  it("explains a disabled record store on a 503", async () => {
    mockGetPipelineJob.mockRejectedValue(new ApiError(503, "off"));
    renderPage();
    await waitFor(() => expect(screen.getByText("Pipeline data is off")).toBeInTheDocument());
  });

  it("writes a filter to the URL, and the filtered view survives a reload", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("No builds match this filter.")).toBeInTheDocument());

    fireEvent.change(screen.getByLabelText("Branch filter"), { target: { value: "main" } });
    await waitFor(() => expect(screen.getByTestId("location").textContent).toContain("branch=main"));

    const reloadedUrl = screen.getByTestId("location").textContent!;

    // A fresh mount at the URL the filter produced — the same shape a
    // browser reload of that URL would present.
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage(reloadedUrl);
    await waitFor(() => {
      const inputs = screen.getAllByLabelText("Branch filter") as HTMLInputElement[];
      expect(inputs[inputs.length - 1].value).toBe("main");
    });
  });

  /** last30DaysRow reads the <dd> immediately after the named <dt> inside
   * the "Last 30 days" card, scoped to that card so "Builds" does not
   * collide with the builds table's section title below it. */
  function last30DaysRow(label: string): string | null {
    const cardTitle = screen.getByText("Last 30 days");
    const card = cardTitle.parentElement?.parentElement;
    if (!card) throw new Error("Last 30 days card not found");
    const dt = within(card).getByText(label);
    return dt.nextElementSibling?.textContent ?? null;
  }

  it("shows Not available on every Last 30 days row when the card is null", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(last30DaysRow("Builds")).toBe("Not available");
    expect(last30DaysRow("Pass rate")).toBe("Not available");
    expect(last30DaysRow("Median")).toBe("Not available");
    expect(last30DaysRow("Median queue wait")).toBe("Not available");
    expect(last30DaysRow("Slowest stage")).toBe("Not available");
  });

  it("renders the Last 30 days card's builds, pass rate, median, and slowest stage", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        last30Days: {
          builds: 12,
          passRate: 0.75,
          medianDurationMs: 65000,
          medianQueueWaitMs: 30000,
          slowestStage: { name: "Deploy", durationMs: 125000 },
        },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(last30DaysRow("Builds")).toBe("12");
    expect(last30DaysRow("Pass rate")).toBe("75%");
    expect(last30DaysRow("Median")).toBe("1m 5s");
    expect(last30DaysRow("Median queue wait")).toBe("30s");
    expect(last30DaysRow("Slowest stage")).toBe("Deploy (2m 5s)");
  });

  it("shows a dash for a Last 30 days row with no finished run or stage", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail({ last30Days: { builds: 0 } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(last30DaysRow("Builds")).toBe("0");
    expect(last30DaysRow("Pass rate")).toBe("—");
    expect(last30DaysRow("Median")).toBe("—");
    expect(last30DaysRow("Median queue wait")).toBe("—");
    expect(last30DaysRow("Slowest stage")).toBe("—");
  });

  it("offers no Run control to a caller with no tenant write grant", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Run" })).not.toBeInTheDocument();
  });

  it("opens the Run drawer for a caller holding the tenant write grant", async () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { write: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetPipelineJob.mockResolvedValue(jobDetail({ parameters: [] }));
    renderPage();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("navigates to the new run with requested state, so the run page starts in the requested window", async () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { write: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetPipelineJob.mockResolvedValue(jobDetail({ parameters: [] }));
    mockCreateRun.mockResolvedValue({ runId: "run-42" });
    renderPage();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    const dialog = screen.getByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Run" }));

    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe(
        "/pipelines/acme/payments%2Fcheckout-api%2Fmain/run-42",
      ),
    );
    expect(screen.getByTestId("location-state").textContent).toBe(JSON.stringify({ requested: true }));
  });

  it("offers no Run control when the write grant's job pattern excludes this job", async () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "acme", jobs: ["allowed/**"], capabilities: { pipelines: { write: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetPipelineJob.mockResolvedValue(jobDetail({ jobPath: "forbidden/job" }));
    renderPage();
    await waitFor(() => expect(screen.getByText("forbidden/job")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Run" })).not.toBeInTheDocument();
  });

  it("offers the Run control when the write grant's job pattern includes this job", async () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "acme", jobs: ["allowed/**"], capabilities: { pipelines: { write: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetPipelineJob.mockResolvedValue(jobDetail({ jobPath: "allowed/x", parameters: [] }));
    renderPage();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeInTheDocument());
  });

  it("offers the Run control for a synthetic tenant via a namespace-scoped grant, even though the URL team segment is only the controller name", async () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [{ namespaces: ["team-pay"], capabilities: { pipelines: { write: true } }, tenants: [] }],
        tenants: [],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({ jobPath: "ci/main", tenantFolder: "", tenant: "team-pay/ci", parameters: [] }),
    );
    renderPage("/pipelines/ci/ci%2Fmain");
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeInTheDocument());
  });
});

describe("PipelinePage — Template row", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockListRuns.mockResolvedValue({ items: [] });
    vi.mocked(usePermissions).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof usePermissions>);
  });

  /** definedByRow reads the <dd> immediately after the named <dt> inside
   * the "Defined by" card, mirroring last30DaysRow's scoping. */
  function definedByRow(label: string): HTMLElement | null {
    const cardTitle = screen.getByText("Defined by");
    const card = cardTitle.parentElement?.parentElement;
    if (!card) throw new Error("Defined by card not found");
    const dt = within(card).getByText(label);
    return dt.nextElementSibling as HTMLElement | null;
  }

  it("names an inline script and drops Not available", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail({ definition: { source: "inline", profile: "2.570" } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("Script")).toBeInTheDocument());
    expect(definedByRow("Script")?.textContent).toBe("Inline, declared by the bundle item.");
    expect(definedByRow("Profile")?.textContent).toBe("2.570");
    expect(screen.queryByText("Jenkinsfile")).not.toBeInTheDocument();
    const card = screen.getByText("Defined by").parentElement?.parentElement;
    expect(within(card as HTMLElement).queryByText("Not available.")).not.toBeInTheDocument();
  });

  it("shows the profile for a template-declared job", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        definition: { source: "template", profile: "2.570" },
        template: { name: "go-service", version: "2.1.4", source: "items" },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Template")).toBeInTheDocument());
    expect(definedByRow("Profile")?.textContent).toBe("2.570");
    expect(screen.queryByText("Script")).not.toBeInTheDocument();
    expect(screen.queryByText("Jenkinsfile")).not.toBeInTheDocument();
  });

  it("prints Not available only with no definition and no template", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("Defined by")).toBeInTheDocument());
    const card = screen.getByText("Defined by").parentElement?.parentElement;
    expect(within(card as HTMLElement).getByText("Not available.")).toBeInTheDocument();
  });

  it("hides the Template row for a job with no catalog template", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(screen.queryByText("Template")).not.toBeInTheDocument();
  });

  it("shows the name and version for an items-declared job", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({ template: { name: "go-service", version: "2.1.4", source: "items" } }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Template")).toBeInTheDocument());
    expect(definedByRow("Template")?.textContent).toBe("go-service v2.1.4");
  });

  it("names the build a marker-defined job's template came from", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        template: {
          name: "go-service",
          version: "2.1.4",
          markerPath: ".varroa/pipeline.yaml",
          markerRevision: "abc1234",
          source: "marker",
          asOfBuild: 87,
        },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Template")).toBeInTheDocument());
    const text = definedByRow("Template")?.textContent ?? "";
    expect(text).toContain("go-service");
    expect(text).toContain("2.1.4");
    expect(text).toContain(".varroa/pipeline.yaml");
    expect(text).toContain("87");
  });

  it("explains a marker-defined job that has never run", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({ template: { markerPath: ".varroa/pipeline.yaml", source: "marker" } }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Template")).toBeInTheDocument());
    const text = definedByRow("Template")?.textContent ?? "";
    expect(text).toContain(".varroa/pipeline.yaml");
    expect(text).toContain("Resolves at the first build.");
  });
});

describe("PipelinePage — dependency edges", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockListRuns.mockResolvedValue({ items: [] });
    vi.mocked(usePermissions).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof usePermissions>);
  });

  /** triggersRow reads the <dd> immediately after the named <dt> inside
   * the "Triggers" card, mirroring last30DaysRow's scoping. */
  function triggersRow(label: string): HTMLElement | null {
    const cardTitle = screen.getByText("Triggers");
    const card = cardTitle.parentElement?.parentElement;
    if (!card) throw new Error("Triggers card not found");
    const dt = within(card).getByText(label);
    return dt.nextElementSibling as HTMLElement | null;
  }

  it("hides the Upstream and Downstream rows for a job with no edge", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(screen.queryByText("Upstream")).not.toBeInTheDocument();
    expect(screen.queryByText("Downstream")).not.toBeInTheDocument();
  });

  it("shows a granted upstream edge as tenant/job with its threshold, linked to the job's page", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        dependencies: {
          upstream: [{ tenant: "alpha", job: "release", result: "SUCCESS", granted: true }],
          downstream: [],
        },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Upstream")).toBeInTheDocument());
    const row = triggersRow("Upstream");
    expect(row?.textContent).toContain("alpha/release");
    expect(row?.textContent).toContain("SUCCESS");
    expect(row?.textContent).not.toContain("grant missing");
    const link = within(row!).getByRole("link", { name: "alpha/release" });
    expect(link.getAttribute("href")).toBe("/pipelines/alpha/release");
  });

  it("marks an ungranted downstream edge with grant missing, without hiding it", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        dependencies: {
          upstream: [],
          downstream: [{ tenant: "search", job: "rebuild", result: "SUCCESS", granted: false }],
        },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Downstream")).toBeInTheDocument());
    const row = triggersRow("Downstream");
    expect(row?.textContent).toContain("search/rebuild");
    expect(row?.textContent).toContain("grant missing");
    expect(screen.queryByText("Upstream")).not.toBeInTheDocument();
  });
});

describe("PipelinePage — live run events", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    mockListRuns.mockResolvedValue({ items: [] });
    vi.mocked(usePermissions).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof usePermissions>);
  });

  /** runStream makes the job page read one frame of its own job off the
   * team stream. */
  function runStream(frame: RunEvent | null, readyState = "closed") {
    mockUseEventStream.mockImplementation((_baseUrl, scope) =>
      (scope ?? "").startsWith("pipelines:")
        ? { lastEvent: frame, readyState, error: null }
        : { lastEvent: null, readyState: "closed", error: null },
    );
  }

  it("refetches the builds table, and not the job read, for a matching running event", async () => {
    const { repaint } = renderPage();
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(mockGetPipelineJob).toHaveBeenCalledTimes(1));

    runStream({ type: "run", data: notification({ status: "RUNNING" }) });
    repaint();

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(2));
    expect(mockGetPipelineJob).toHaveBeenCalledTimes(1);
  });

  it("refetches both reads for a completed event", async () => {
    const { repaint } = renderPage();
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));

    runStream({ type: "run", data: notification({ status: "COMPLETED", result: "SUCCESS" }) });
    repaint();

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(mockGetPipelineJob).toHaveBeenCalledTimes(2));
  });

  it("refetches nothing for an event of another job", async () => {
    const { repaint } = renderPage();
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));

    runStream({
      type: "run",
      data: notification({ job_path: "payments/other/main", status: "COMPLETED" }),
    });
    repaint();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockListRuns).toHaveBeenCalledTimes(1);
    expect(mockGetPipelineJob).toHaveBeenCalledTimes(1);
  });
});
