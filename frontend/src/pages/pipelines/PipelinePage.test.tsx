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
  teamRunStreamUrl: (team: string) => `/api/v1/pipelines/${encodeURIComponent(team)}/stream`,
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
  pending: false,
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

  it("lists waiting runs", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        waiting: [
          { runId: "run-9", kind: "tenant", limit: "deploys", max: 1, running: 1, since: "2026-10-12T01:00:00Z" },
        ],
      }),
    );
    renderPage();
    await screen.findByText("Waiting");
    expect(screen.getByText("deploys")).toBeInTheDocument();
    expect(screen.getByText(/1 of 1 builds run/)).toBeInTheDocument();
    expect(screen.getByText("Run run-9")).toBeInTheDocument();
  });

  it("sets the document title from the job", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(document.title).toBe("payments/checkout-api/main · acme"));
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

  it("shows the pending note and keeps Run enabled for a pending job", async () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { write: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetPipelineJob.mockResolvedValue(jobDetail({ pending: true }));
    renderPage();
    await screen.findByRole("note");
    expect(screen.getByRole("note")).toHaveTextContent("but no cell has created it yet");
    expect(screen.getByRole("button", { name: "Run" })).toBeEnabled();
  });

  it("shows no pending note when pending is false", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    renderPage();
    await waitFor(() => expect(document.title).toBe("payments/checkout-api/main · acme"));
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
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
          upstream: [{ tenant: "alpha", job: "release", jobPath: "alpha/release", result: "SUCCESS", granted: true }],
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
    expect(link.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Frelease");
  });

  it("links a downstream edge to the same URL the team home uses for that job", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        dependencies: {
          upstream: [],
          downstream: [
            { tenant: "smoke-b", job: "xt-downstream", jobPath: "smoke-b/xt-downstream", result: "SUCCESS", granted: true },
          ],
        },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Downstream")).toBeInTheDocument());
    const row = triggersRow("Downstream");
    const link = within(row!).getByRole("link", { name: "smoke-b/xt-downstream" });
    expect(link.getAttribute("href")).toBe("/pipelines/smoke-b/smoke-b%2Fxt-downstream");
  });

  it("links an edge's last run to its build page", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        dependencies: {
          upstream: [
            {
              tenant: "alpha",
              job: "release",
              jobPath: "alpha/release",
              result: "SUCCESS",
              granted: true,
              lastRun: { tenant: "alpha", jobPath: "alpha/release", runId: "run-up-1", buildNumber: 9 },
            },
          ],
          downstream: [],
        },
      }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Upstream")).toBeInTheDocument());
    const row = triggersRow("Upstream");
    expect(row?.textContent).toContain("last run #9");
    const link = within(row!).getByRole("link", { name: "#9" });
    expect(link.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Frelease/run-up-1");
  });

  it("marks an ungranted downstream edge with grant missing, without hiding it", async () => {
    mockGetPipelineJob.mockResolvedValue(
      jobDetail({
        dependencies: {
          upstream: [],
          downstream: [{ tenant: "search", job: "rebuild", jobPath: "search/rebuild", result: "SUCCESS", granted: false }],
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

  it("shows Webhook Yes for a job with a generic trigger", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail({ triggers: { webhook: true } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("Webhook")).toBeInTheDocument());
    expect(triggersRow("Webhook")?.textContent).toBe("Yes");
  });

  it("shows Webhook No when the job declares no generic trigger", async () => {
    mockGetPipelineJob.mockResolvedValue(jobDetail({ triggers: { push: true } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("Webhook")).toBeInTheDocument());
    expect(triggersRow("Webhook")?.textContent).toBe("No");
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

describe("PipelinePage — wait events", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockGetPipelineJob.mockResolvedValue(jobDetail());
    mockListRuns.mockResolvedValue({ items: [] });
    vi.mocked(usePermissions).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof usePermissions>);
  });

  function runStream(frame: RunEvent | null, readyState = "closed") {
    mockUseEventStream.mockImplementation((_baseUrl, scope) =>
      (scope ?? "").startsWith("pipelines:")
        ? { lastEvent: frame, readyState, error: null }
        : { lastEvent: null, readyState: "closed", error: null },
    );
  }

  it("subscribes to wait frames", async () => {
    renderPage();
    await waitFor(() => expect(mockUseEventStream).toHaveBeenCalled());
    const teamCall = mockUseEventStream.mock.calls.find(([, scope]) => (scope ?? "").startsWith("pipelines:"));
    expect(teamCall?.[2]).toContain("wait");
  });

  it("refetches the job read and the builds table for a wait event of its own job", async () => {
    const { repaint } = renderPage();
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(mockGetPipelineJob).toHaveBeenCalledTimes(1));

    runStream({ type: "wait", data: notification({ kind: "wait", state: "waiting" }) });
    repaint();

    await waitFor(() => expect(mockGetPipelineJob).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(2));
  });

  it("ignores a wait event of another job", async () => {
    const { repaint } = renderPage();
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(mockGetPipelineJob).toHaveBeenCalledTimes(1));

    runStream({
      type: "wait",
      data: notification({ kind: "wait", job_path: "payments/other/main", state: "waiting" }),
    });
    repaint();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockListRuns).toHaveBeenCalledTimes(1);
    expect(mockGetPipelineJob).toHaveBeenCalledTimes(1);
  });
});

describe("PipelinePage — provenance rows", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockListRuns.mockResolvedValue({ items: [] });
    vi.mocked(usePermissions).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof usePermissions>);
  });

  /** definedByCard returns the "Defined by" card, so every absence
   * assertion stays inside that card. */
  function definedByCard(): HTMLElement {
    const cardTitle = screen.getByText("Defined by");
    const card = cardTitle.parentElement?.parentElement;
    if (!card) throw new Error("Defined by card not found");
    return card;
  }

  /** definedByRow reads the <dd> immediately after the named <dt> inside
   * the "Defined by" card, mirroring the other describe blocks. */
  function definedByRow(label: string): HTMLElement | null {
    const dt = within(definedByCard()).getByText(label);
    return dt.nextElementSibling as HTMLElement | null;
  }

  /** declaredByGitSource builds one git-declared input, with the
   * forge-shaped repository the link cases expect. */
  function declaredByGitSource(overrides: Record<string, unknown> = {}) {
    return {
      kind: "gitSource",
      name: "https://github.com/example/casc-bundles",
      repoURL: "https://github.com/example/casc-bundles",
      path: "bundles/team-platform",
      revision: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
      bundle: { namespace: "teams-platform", name: "platform-baseline" },
      ...overrides,
    };
  }

  /** renderJobWithDefinition opens the page on a job detail carrying the
   * given definition object. */
  async function renderJobWithDefinition(definition: Record<string, unknown>) {
    mockGetPipelineJob.mockResolvedValue(jobDetail({ definition }));
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
  }

  it("links the Bundle row to the composed bundle page", async () => {
    await renderJobWithDefinition({ source: "jenkinsfile", declaredBy: declaredByGitSource() });

    const link = within(definedByRow("Bundle")!).getByRole("link", { name: "platform-baseline" });
    expect(link.getAttribute("href")).toBe("/catalog/bundles/teams-platform/platform-baseline");
  });

  it("links the Source input row for a GitHub gitSource and truncates the revision", async () => {
    await renderJobWithDefinition({ source: "jenkinsfile", declaredBy: declaredByGitSource() });

    const row = definedByRow("Source input")!;
    const link = within(row).getByRole("link", { name: /casc-bundles/ });
    expect(link.getAttribute("href")).toBe(
      "https://github.com/example/casc-bundles/tree/9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
    );
    expect(link.textContent).toContain("9f1c2d3e4a5b");
    expect(link.textContent).not.toContain("9f1c2d3e4a5b6");
  });

  it("shows an ociSource as text with no link", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      declaredBy: {
        kind: "ociSource",
        name: "ghcr.io/varroaci/casc-bundles:v1",
        ociRef: "ghcr.io/varroaci/casc-bundles:v1",
        path: "bundles/team-platform",
        revision: "sha256:1f2e3d4c5b6a79880796a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4",
        bundle: { namespace: "teams-platform", name: "platform-baseline" },
      },
    });

    const row = definedByRow("Source input")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toContain("ghcr.io/varroaci/casc-bundles:v1");
    expect(row.textContent).toContain("@ sha256:1f2e3");
    expect(row.textContent).not.toContain("sha256:1f2e3d");
  });

  it("shows a non-forge gitSource as text with no link", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      declaredBy: declaredByGitSource({
        name: "https://git.example.com/team/casc-bundles",
        repoURL: "https://git.example.com/team/casc-bundles",
      }),
    });

    const row = definedByRow("Source input")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toContain("https://git.example.com/team/casc-bundles");
    expect(row.textContent).toContain("@ 9f1c2d3e4a5b");
  });

  it("shows an itemRef's catalog item name and namespace with no link", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      declaredBy: {
        kind: "itemRef",
        name: "standard-theme",
        revision: "sha256:1f2e3d4c5b6a79880796a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4",
        bundle: { namespace: "teams-platform", name: "platform-baseline" },
        catalogItem: { name: "standard-theme", namespace: "catalog" },
      },
    });

    const row = definedByRow("Source input")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("standard-theme (catalog)");
  });

  it("falls back to the input name when an itemRef carries no catalogItem object", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      declaredBy: {
        kind: "itemRef",
        name: "standard-theme",
        bundle: { namespace: "teams-platform", name: "platform-baseline" },
      },
    });

    const row = definedByRow("Source input")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("standard-theme");
  });

  it("falls back to the input name when a gitSource carries no repoURL", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      declaredBy: {
        kind: "gitSource",
        name: "example/casc-bundles",
        bundle: { namespace: "teams-platform", name: "platform-baseline" },
      },
    });

    const row = definedByRow("Source input")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("example/casc-bundles");
  });

  it("shows neither provenance row without a declaredBy object", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      jenkinsfileRepo: "https://github.com/example/app",
      jenkinsfileCommit: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
      definitionPath: "Jenkinsfile",
    });

    const card = definedByCard();
    expect(within(card).queryByText("Bundle")).not.toBeInTheDocument();
    expect(within(card).queryByText("Source input")).not.toBeInTheDocument();
  });

  it("links the Jenkinsfile row when all four conditions hold", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      jenkinsfileRepo: "https://github.com/example/app",
      jenkinsfileCommit: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
      definitionPath: "Jenkinsfile",
    });

    const link = within(definedByRow("Jenkinsfile")!).getByRole("link");
    expect(link.getAttribute("href")).toBe(
      "https://github.com/example/app/blob/9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293/Jenkinsfile",
    );
    expect(link.textContent).toBe("https://github.com/example/app @ 9f1c2d3");
  });

  it("keeps the Jenkinsfile row as plain text when the script path is absent", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      jenkinsfileRepo: "https://github.com/example/app",
      jenkinsfileCommit: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
    });

    const row = definedByRow("Jenkinsfile")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("https://github.com/example/app @ 9f1c2d3");
  });

  it("keeps the Jenkinsfile row as plain text for a repository outside a known forge", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      jenkinsfileRepo: "https://git.example.com/team/app",
      jenkinsfileCommit: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
      definitionPath: "Jenkinsfile",
    });

    const row = definedByRow("Jenkinsfile")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("https://git.example.com/team/app @ 9f1c2d3");
  });

  it("keeps the Jenkinsfile row as plain text when the commit is absent", async () => {
    await renderJobWithDefinition({
      source: "jenkinsfile",
      jenkinsfileRepo: "https://github.com/example/app",
      definitionPath: "Jenkinsfile",
    });

    const row = definedByRow("Jenkinsfile")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("https://github.com/example/app");
  });

  it("never links the Jenkinsfile row for a template source", async () => {
    await renderJobWithDefinition({
      source: "template",
      jenkinsfileRepo: "https://github.com/example/app",
      jenkinsfileCommit: "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293",
      definitionPath: "Jenkinsfile",
      profile: "2.570",
    });

    const row = definedByRow("Jenkinsfile")!;
    expect(within(row).queryByRole("link")).toBeNull();
    expect(row.textContent).toBe("https://github.com/example/app @ 9f1c2d3");
  });
});

describe("PipelinePage — run table links", () => {
  beforeEach(() => {
    mockGetPipelineJob.mockReset();
    mockListRuns.mockReset();
    mockGetPipelineJob.mockResolvedValue(jobDetail());
  });

  const oneRun = (overrides: Record<string, unknown> = {}) => ({
    runId: "run-1",
    buildNumber: 42,
    status: "COMPLETED",
    result: "SUCCESS",
    branch: "main",
    scmRevision: "9f1c2d3aaaaaaaaa",
    cause: "Push",
    queuedAt: "2026-10-11T10:00:00Z",
    startedAt: "2026-10-11T10:00:30Z",
    durationMs: 60000,
    ...overrides,
  });

  it("links the commit cell to the forge commit for a GitHub scmUrl", async () => {
    mockListRuns.mockResolvedValue({ items: [oneRun({ scmUrl: "https://github.com/example/app" })] });
    renderPage();
    const link = await screen.findByRole("link", { name: "9f1c2d3" });
    expect(link).toHaveAttribute("href", "https://github.com/example/app/commit/9f1c2d3aaaaaaaaa");
  });

  it("shows a copy button for a non-forge commit", async () => {
    mockListRuns.mockResolvedValue({ items: [oneRun({ scmUrl: "https://git.example.org/x/y" })] });
    renderPage();
    expect(await screen.findByRole("button", { name: "Copy commit 9f1c2d3aaaaaaaaa" })).toBeInTheDocument();
  });

  it("links the branch cell to the branch-filtered job page", async () => {
    mockListRuns.mockResolvedValue({ items: [oneRun()] });
    renderPage();
    const link = await screen.findByRole("link", { name: "main" });
    expect(link).toHaveAttribute("href", "/pipelines/acme/payments%2Fcheckout-api%2Fmain?branch=main");
  });

  it("marks a pull-request run with its change request beside the branch", async () => {
    mockListRuns.mockResolvedValue({
      items: [
        oneRun({
          branch: "fix/cache",
          changeRequest: { id: "12", title: "Fix the cache", url: "https://forge.example/pr/12" },
        }),
      ],
    });
    renderPage();
    const chip = await screen.findByRole("link", { name: "PR #12 Fix the cache" });
    expect(chip).toHaveTextContent("PR #12");
    expect(chip).toHaveAttribute("href", "https://forge.example/pr/12");
  });

  it("shows the queue wait column", async () => {
    mockListRuns.mockResolvedValue({ items: [oneRun()] });
    renderPage();
    const header = await screen.findByRole("columnheader", { name: "Queue" });
    const cell = header.closest("table")!.querySelector("tbody tr td:nth-child(7)")!;
    expect(cell.textContent).toBe("30s");
  });
});
