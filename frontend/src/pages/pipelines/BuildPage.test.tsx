import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { InitialEntry } from "react-router-dom";
import BuildPage from "./BuildPage";
import { ApiError } from "../../hooks/useApi";
import type { RunDetail } from "../../types";

const mockGetRun = vi.fn();
const mockListRunArtifacts = vi.fn();
const mockListRunTests = vi.fn();
const mockListRunChanges = vi.fn();
const mockListRuns = vi.fn();
const mockCancelRun = vi.fn();
const mockRerunRun = vi.fn();
const mockDownloadRunArtifact = vi.fn();
const mockUseEventStream = vi.fn(
  (
    _baseUrl?: string | null,
    _scope?: string | null,
    _eventNames?: string[],
    _options?: { resumeUrl?: () => string | null },
  ): { lastEvent: unknown; readyState: string; error: Error | null } => ({
    lastEvent: null,
    readyState: "closed",
    error: null,
  }),
);

vi.mock("../../api/client", async () => {
  const actual = await vi.importActual<typeof import("../../api/client")>("../../api/client");
  return {
    ...actual,
    getRun: (...args: unknown[]) => mockGetRun(...args),
    listRunArtifacts: (...args: unknown[]) => mockListRunArtifacts(...args),
    listRunTests: (...args: unknown[]) => mockListRunTests(...args),
    listRunChanges: (...args: unknown[]) => mockListRunChanges(...args),
    listRuns: (...args: unknown[]) => mockListRuns(...args),
    cancelRun: (...args: unknown[]) => mockCancelRun(...args),
    rerunRun: (...args: unknown[]) => mockRerunRun(...args),
    downloadRunArtifact: (...args: unknown[]) => mockDownloadRunArtifact(...args),
  };
});

vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: (...args: Parameters<typeof mockUseEventStream>) => mockUseEventStream(...args),
}));

const mockToast = vi.fn();
vi.mock("../../components/Toast", () => ({
  useToast: () => ({ toast: mockToast }),
}));

// canAccessTenantPipeline runs the real folder-relative matching logic; only
// usePermissions is mocked, so each test's fixture proves the actual rule
// rather than a stand-in boolean.
vi.mock("../../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../../hooks/usePermissions")>(
    "../../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});
import { usePermissions } from "../../hooks/usePermissions";

/** grantTenantPipelinesWrite makes usePermissions report a "payments"
 * tenant write grant scoped to the given folder-relative job globs (no
 * globs grants the whole tenant, matching the previous test default). */
function grantTenantPipelinesWrite(jobs: string[] = []) {
  vi.mocked(usePermissions).mockReturnValue({
    data: {
      global: {},
      scopes: [],
      tenants: [{ tenant: "payments", jobs, capabilities: { pipelines: { write: true } } }],
    },
  } as unknown as ReturnType<typeof usePermissions>);
}

const mockNavigate = vi.fn();
const mockUseParams = vi.fn(() => ({ team: "payments", job: "checkout-api/main", run: "run-1" }));
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return {
    ...actual,
    useParams: () => mockUseParams(),
    useNavigate: () => mockNavigate,
  };
});

function baseRun(overrides: Partial<RunDetail> = {}): RunDetail {
  return {
    runId: "run-1",
    jobPath: "checkout-api/main",
    tenantFolder: "",
    tenant: "payments",
    status: "COMPLETED",
    buildNumber: 42,
    branch: "main",
    scmRevision: "abcdef1234567890",
    result: "SUCCESS",
    startedAt: "2026-01-01T00:00:00Z",
    durationMs: 12000,
    actor: "nate",
    cause: "Push",
    logFinalized: true,
    logComplete: true,
    stages: [],
    ...overrides,
  };
}

/** renderPage returns repaint, which re-renders the page so it reads the
 * stream state the test just set. */
function renderPage(entry: InitialEntry = "/pipelines/payments/checkout-api%2Fmain/run-1") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = () => (
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[entry]}>
        <BuildPage />
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(tree());
  return { ...view, repaint: () => view.rerender(tree()) };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockUseParams.mockReturnValue({ team: "payments", job: "checkout-api/main", run: "run-1" });
  grantTenantPipelinesWrite();
  mockListRunArtifacts.mockResolvedValue({ items: [] });
  mockListRunTests.mockResolvedValue({ items: [{ suite: "_total", total: 0, failed: 0, skipped: 0 }] });
  mockListRunChanges.mockResolvedValue({ items: [] });
  mockListRuns.mockResolvedValue({ items: [] });
  mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
});

describe("BuildPage — record and recovery states", () => {
  it("renders the forbidden state", async () => {
    mockGetRun.mockRejectedValue(new ApiError(403, "forbidden"));
    renderPage();
    expect(await screen.findByText("Access denied")).toBeInTheDocument();
  });

  it("renders a generic error state", async () => {
    mockGetRun.mockRejectedValue(new ApiError(500, "boom"));
    renderPage();
    expect(await screen.findByText("Unable to load page")).toBeInTheDocument();
  });

  it("renders the 503 record-store-off state", async () => {
    mockGetRun.mockRejectedValue(new ApiError(503, "record store not configured"));
    renderPage();
    expect(await screen.findByText("Pipeline history is off")).toBeInTheDocument();
  });

  it("renders the four tabs, the disabled download, and the tests notice", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockListRunArtifacts.mockResolvedValue({
      items: [
        { path: "build/out.jar", sizeBytes: 100, downloadable: true },
        { path: "build/local.log", sizeBytes: 50, downloadable: false },
      ],
    });
    mockListRunTests.mockResolvedValue({
      items: [
        { suite: "_total", total: 10, failed: 1, skipped: 0 },
        { suite: "unit", total: 10, failed: 1, skipped: 0 },
      ],
    });
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByRole("button", { name: /^Log/ })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^Artifacts/ }));
    expect(await screen.findByText("build/out.jar")).toBeInTheDocument();
    const downloadButtons = screen.getAllByRole("button", { name: "Download" });
    expect(downloadButtons[1]).toBeDisabled();
    expect(downloadButtons[0]).not.toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: /^Tests/ }));
    expect(await screen.findByText("Individual test cases are not stored.")).toBeInTheDocument();
    expect(screen.getByText("unit")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^Changes/ }));
  });
});

describe("BuildPage — pending and not-found states", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows Requested, then Not started after 60 seconds, while still polling", async () => {
    mockGetRun.mockRejectedValue(new ApiError(404, "not found"));
    renderPage({ pathname: "/pipelines/payments/checkout-api%2Fmain/run-1", state: { requested: true } });

    expect(await screen.findByText("Requested")).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(61_000);
    });
    expect(screen.getByText("Not started")).toBeInTheDocument();
    expect(screen.getByText("The reason is not available.")).toBeInTheDocument();
    expect(mockGetRun.mock.calls.length).toBeGreaterThan(15);
  });

  it("shows not-found with a hint on a cold load, and replaces it when a record arrives", async () => {
    mockGetRun.mockRejectedValue(new ApiError(404, "not found"));
    renderPage();

    expect(await screen.findByText("Run not found")).toBeInTheDocument();
    expect(screen.getByText("This run may still be starting.")).toBeInTheDocument();

    mockGetRun.mockResolvedValue(baseRun());
    await act(async () => {
      await vi.advanceTimersByTimeAsync(9_000);
    });
    expect(await screen.findByText(/Build #42/)).toBeInTheDocument();
  });

  it("shows the interrupted banner with a retry link, and disables re-run once a retry exists", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ result: "INTERRUPTED", interruptedReason: "node lost", retryRunId: "run-2" }),
    );
    renderPage();
    await screen.findByText(/Build #42/);
    expect(screen.getByText(/Interrupted: node lost/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View retry" })).toHaveAttribute(
      "href",
      "/pipelines/payments/checkout-api%2Fmain/run-2",
    );
    expect(screen.getByRole("button", { name: /Re-run/ })).toBeDisabled();
  });

  it("shows the interrupted banner's fallback text when the run carries no reason", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "INTERRUPTED" }));
    renderPage();
    await screen.findByText(/Build #42/);
    expect(screen.getByText("Interrupted: reason not available.")).toBeInTheDocument();
  });

  it("names a build that never started when the interrupted run holds no start time", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ result: "INTERRUPTED", interruptedReason: "cell-rolled", buildNumber: undefined, startedAt: undefined }),
    );
    renderPage();
    await screen.findByText(/Interrupted: cell-rolled/);
    expect(screen.getByText("The build never started on the cell.")).toBeInTheDocument();
    expect(
      screen.getByText("Re-run starts with each parameter's declared default."),
    ).toBeInTheDocument();
  });

  it("says neither sentence for an interrupted run that started, even with no build number", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ result: "INTERRUPTED", interruptedReason: "no-heartbeat", buildNumber: undefined }),
    );
    renderPage();
    await screen.findByText(/Interrupted: no-heartbeat/);
    expect(screen.queryByText("The build never started on the cell.")).not.toBeInTheDocument();
    expect(
      screen.queryByText("Re-run starts with each parameter's declared default."),
    ).not.toBeInTheDocument();
  });
});

describe("BuildPage — log stream resume", () => {
  function latestResumeUrl(): () => string | null {
    const calls = mockUseEventStream.mock.calls;
    const options = calls[calls.length - 1]?.[3];
    if (!options?.resumeUrl) throw new Error("useEventStream was not called with a resumeUrl option");
    return options.resumeUrl;
  }

  it("passes useEventStream a resumeUrl with no offset before any log frame arrives", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log");
  });

  it("carries one byte past the last line frame from the last line frame on reconnect", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    mockUseEventStream.mockReturnValue({
      lastEvent: { type: "line", data: { n: 7, off: 512, ts: 1700000000000, text: "building" } },
      readyState: "open",
      error: null,
    });
    // Any state change re-renders BuildPage, which calls useEventStream again
    // (picking up the new mocked lastEvent) and flushes the effect that
    // tracks the watermark from it.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Artifacts/ }));
    });

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log?from=513&fromLine=8");
  });

  it("carries from=<offset> with no fromLine from the last gap frame on reconnect", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    mockUseEventStream.mockReturnValue({
      lastEvent: { type: "gap", data: { seq: 4, off: 900 } },
      readyState: "open",
      error: null,
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Artifacts/ }));
    });

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log?from=900");
  });
});

describe("BuildPage — stage graph identity across re-renders", () => {
  it("keeps a live-merged stage across an unrelated re-render when the run has no stage list yet", async () => {
    mockGetRun.mockResolvedValue(baseRun({ stages: undefined, status: "RUNNING" }));
    renderPage();
    await screen.findByText(/Build #42/);

    mockUseEventStream.mockReturnValue({
      lastEvent: {
        type: "stage",
        data: { stageId: "s1", name: "Build", status: "IN_PROGRESS", startOffset: 0, startLine: 0 },
      },
      readyState: "open",
      error: null,
    });
    // Any state change re-renders BuildPage and hands StageGraph the
    // stage event via lastEvent, matching the log-stream-resume tests above.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Artifacts/ }));
    });
    expect(document.getElementById("node-stage-s1")).toBeTruthy();

    // A second, unrelated re-render — the same lastEvent, a different tab
    // click — must not wipe the stage the live event already merged.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Tests/ }));
    });
    expect(document.getElementById("node-stage-s1")).toBeTruthy();
  });
});

describe("BuildPage — folder-relative write authorization", () => {
  it("renders Cancel and Re-run for a run inside the granted folder", async () => {
    grantTenantPipelinesWrite(["checkout-api/**"]);
    mockGetRun.mockResolvedValue(
      baseRun({ tenantFolder: "payments", jobPath: "payments/checkout-api/main", status: "RUNNING" }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByRole("button", { name: /Cancel/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Re-run/ })).toBeInTheDocument();
  });

  it("hides Cancel and Re-run for a run outside the granted folder", async () => {
    grantTenantPipelinesWrite(["other/**"]);
    mockGetRun.mockResolvedValue(
      baseRun({ tenantFolder: "payments", jobPath: "payments/checkout-api/main", status: "RUNNING" }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByRole("button", { name: /Cancel/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Re-run/ })).not.toBeInTheDocument();
  });

  it("renders Cancel and Re-run for a synthetic tenant via a namespace-scoped grant, even though the URL team segment is only the controller name", async () => {
    mockUseParams.mockReturnValue({ team: "ci", job: "main", run: "run-1" });
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [{ namespaces: ["team-pay"], capabilities: { pipelines: { write: true } }, tenants: [] }],
        tenants: [],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetRun.mockResolvedValue(
      baseRun({ tenantFolder: "", tenant: "team-pay/ci", jobPath: "main", status: "RUNNING" }),
    );
    renderPage("/pipelines/ci/main/run-1");
    await screen.findByText(/Build #42/);

    expect(screen.getByRole("button", { name: /Cancel/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Re-run/ })).toBeInTheDocument();
  });

  it("hides Cancel and Re-run for a run tenanted team-b opened under /pipelines/team-a/..., when the caller writes only team-a", async () => {
    mockUseParams.mockReturnValue({ team: "team-a", job: "checkout-api/main", run: "run-1" });
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "team-a", jobs: [], capabilities: { pipelines: { write: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
    mockGetRun.mockResolvedValue(
      baseRun({ tenantFolder: "team-b", tenant: "team-b", jobPath: "team-b/checkout-api/main", status: "RUNNING" }),
    );
    renderPage("/pipelines/team-a/checkout-api%2Fmain/run-1");
    await screen.findByText(/Build #42/);

    expect(screen.queryByRole("button", { name: /Cancel/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Re-run/ })).not.toBeInTheDocument();
  });
});

describe("BuildPage — a refused build trigger", () => {
  function triggerFailedRun(reasonCode: string): RunDetail {
    return baseRun({
      result: "TRIGGER_FAILED",
      buildNumber: undefined,
      durationMs: undefined,
      startedAt: undefined,
      triggerError: "trigger build checkout-api/main: HTTP 404: no such job",
      triggerReasonCode: reasonCode,
    });
  }

  const sentences: Array<[string, string]> = [
    ["JOB_NOT_FOUND", "The job does not exist on the controller."],
    ["JOB_NOT_BUILDABLE", "The job cannot start a build."],
    ["TRIGGER_REJECTED", "The controller refused the trigger."],
  ];

  it.each(sentences)("names the reason for %s", async (reasonCode, sentence) => {
    mockGetRun.mockResolvedValue(triggerFailedRun(reasonCode));
    renderPage();

    expect(await screen.findByText(sentence)).toBeInTheDocument();
    expect(
      screen.getByText("trigger build checkout-api/main: HTTP 404: no such job"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Build #/)).not.toBeInTheDocument();
    expect(screen.queryByText("Pipeline")).not.toBeInTheDocument();
  });

  it("says that Re-run starts with each declared default", async () => {
    mockGetRun.mockResolvedValue(triggerFailedRun("JOB_NOT_FOUND"));
    renderPage();

    expect(await screen.findByText(/declared default/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Re-run/ })).toBeInTheDocument();
  });

  it("replaces the requested state as soon as the record arrives", async () => {
    mockGetRun.mockResolvedValue(triggerFailedRun("JOB_NOT_FOUND"));
    renderPage({ pathname: "/pipelines/payments/checkout-api%2Fmain/run-1", state: { requested: true } });

    expect(await screen.findByText("The job does not exist on the controller.")).toBeInTheDocument();
    expect(screen.queryByText("Requested")).not.toBeInTheDocument();
    expect(screen.queryByText("Not started")).not.toBeInTheDocument();
  });

  it("shows the same card on a cold load", async () => {
    mockGetRun.mockResolvedValue(triggerFailedRun("TRIGGER_REJECTED"));
    renderPage();

    expect(await screen.findByText("The controller refused the trigger.")).toBeInTheDocument();
    expect(screen.queryByText("Run not found")).not.toBeInTheDocument();
  });
});

describe("BuildPage — live run events", () => {
  /** notification builds one frame of the team run stream. */
  function notification(overrides: Record<string, unknown> = {}) {
    return {
      kind: "run",
      cluster: "core",
      tenant: "payments",
      job_path: "checkout-api/main",
      run_id: "run-1",
      seq: 1,
      at: "2026-01-01T00:00:00Z",
      ...overrides,
    };
  }

  /** runStream makes the page read the given frame off the team run stream,
   * and holds the run log stream closed. */
  function runStream(frame: { type: string; data: unknown } | null, readyState = "closed") {
    mockUseEventStream.mockImplementation((_baseUrl, scope) =>
      scope === "pipelines:payments"
        ? { lastEvent: frame, readyState, error: null }
        : { lastEvent: null, readyState: "closed", error: null },
    );
  }

  it("refetches the record for a matching run event", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);
    const before = mockGetRun.mock.calls.length;

    runStream({ type: "run", data: notification() });
    await act(async () => {
      repaint();
    });

    expect(mockGetRun.mock.calls.length).toBeGreaterThan(before);
  });

  it("refetches the record for a matching stage event, and renders the stage the record carries", async () => {
    mockGetRun.mockResolvedValueOnce(baseRun({ status: "RUNNING", result: undefined, stages: [] }));
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);
    expect(document.getElementById("node-stage-s1")).toBeNull();

    mockGetRun.mockResolvedValue(
      baseRun({
        status: "RUNNING",
        result: undefined,
        stages: [{ stageId: "s1", name: "Deploy", status: "IN_PROGRESS" }],
      }),
    );
    runStream({ type: "stage", data: notification({ kind: "stage" }) });
    await act(async () => {
      repaint();
    });

    expect(document.getElementById("node-stage-s1")).toBeTruthy();
  });

  it("refetches nothing for an event of another run", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);
    const before = mockGetRun.mock.calls.length;

    runStream({ type: "run", data: notification({ run_id: "run-9" }) });
    await act(async () => {
      repaint();
    });
    await act(async () => {
      await Promise.resolve();
    });

    expect(mockGetRun.mock.calls.length).toBe(before);
  });

  it("polls the record while the stream is not open and the run is not terminal", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetRun.mockResolvedValue(baseRun({ status: "RUNNING", result: undefined }));
      runStream(null, "closed");
      renderPage();
      await screen.findByText(/Build #42/);
      const before = mockGetRun.mock.calls.length;

      await act(async () => {
        await vi.advanceTimersByTimeAsync(6_000);
      });

      expect(mockGetRun.mock.calls.length).toBeGreaterThanOrEqual(before + 2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("runs no poll while the stream is open", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetRun.mockResolvedValue(baseRun({ status: "RUNNING", result: undefined }));
      runStream(null, "open");
      renderPage();
      await screen.findByText(/Build #42/);
      const settled = mockGetRun.mock.calls.length;

      await act(async () => {
        await vi.advanceTimersByTimeAsync(9_000);
      });

      expect(mockGetRun.mock.calls.length).toBe(settled);
    } finally {
      vi.useRealTimers();
    }
  });
});
