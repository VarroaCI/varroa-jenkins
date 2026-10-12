import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { InitialEntry } from "react-router-dom";
import BuildPage from "./BuildPage";
import { ApiError } from "../../hooks/useApi";
import type { RunDetail, RunInput, RunScript, RunTestListResponse } from "../../types";

const mockGetRun = vi.fn();
const mockListRunArtifacts = vi.fn();
const mockListRunTests = vi.fn();
const mockListRunChanges = vi.fn();
const mockListRuns = vi.fn();
const mockCancelRun = vi.fn();
const mockRerunRun = vi.fn();
const mockProceedRunInput = vi.fn();
const mockAbortRunInput = vi.fn();
const mockDownloadRunArtifact = vi.fn();
const mockGetRunScript = vi.fn();
const mockReplayRun = vi.fn();

// jsdom cannot run a real EditorView. The mock view answers the text the
// test sets in editorText.
let editorText = "";
vi.mock("@codemirror/state", () => ({ EditorState: { create: vi.fn(() => ({})) } }));
vi.mock("@codemirror/view", () => ({
  EditorView: Object.assign(
    class MockEditorView {
      state = { doc: { toString: () => editorText } };
      destroy = vi.fn();
      dispatch = vi.fn();
    },
    { theme: vi.fn(() => []), updateListener: { of: vi.fn(() => []) } },
  ),
  keymap: Object.assign(vi.fn(() => []), { of: vi.fn(() => []) }),
  lineNumbers: vi.fn(() => []),
}));
vi.mock("@codemirror/commands", () => ({ defaultKeymap: [] }));
const mockUseEventStream = vi.fn(
  (
    _baseUrl?: string | null,
    _scope?: string | null,
    _eventNames?: string[],
    _options?: {
      resumeUrl?: () => string | null;
      onEvent?: (ev: { type: string; data: unknown }) => void;
    },
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
    proceedRunInput: (...args: unknown[]) => mockProceedRunInput(...args),
    abortRunInput: (...args: unknown[]) => mockAbortRunInput(...args),
    downloadRunArtifact: (...args: unknown[]) => mockDownloadRunArtifact(...args),
    getRunScript: (...args: unknown[]) => mockGetRunScript(...args),
    replayRun: (...args: unknown[]) => mockReplayRun(...args),
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
  mockListRunTests.mockResolvedValue({ items: [{ suite: "_total", total: 0, failed: 0, skipped: 0 }], cases: [] });
  mockListRunChanges.mockResolvedValue({ items: [] });
  mockListRuns.mockResolvedValue({ items: [] });
  mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
  mockGetRunScript.mockResolvedValue({
    script: "",
    loadedScripts: {},
    definitionKind: "script",
    replayable: false,
    reason: "not a cell run",
  });
});

describe("BuildPage — test cases and the test report", () => {
  /** A tests read whose cases list holds two failed rows and one skipped row.
   * The skipped row sorts first by name, so an order check proves that the
   * page keeps the response order. */
  function testsRead(overrides: Partial<RunTestListResponse> = {}): RunTestListResponse {
    return {
      items: [
        { suite: "_total", total: 12, failed: 2, skipped: 1 },
        { suite: "unit", total: 12, failed: 2, skipped: 1 },
      ],
      cases: [
        {
          suite: "unit",
          className: "com.example.ZedTest",
          name: "zeta",
          status: "FAILED",
          durationMs: 1200,
          errorDetails: "expected 1 but was 2",
          caseKey: "com.example.ZedTest/zeta",
        },
        {
          suite: "unit",
          className: "com.example.MidTest",
          name: "mu",
          status: "FAILED",
          caseKey: "com.example.MidTest/mu",
        },
        {
          suite: "unit",
          className: "com.example.AlphaTest",
          name: "alpha",
          status: "SKIPPED",
          caseKey: "com.example.AlphaTest/alpha",
        },
      ],
      ...overrides,
    };
  }

  async function openTestsTab() {
    await screen.findByText(/Build #42/);
    fireEvent.click(screen.getByRole("tab", { name: /^Tests/ }));
  }

  it("renders the case rows in the response order, above the suite table", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "FAILURE" }));
    mockListRunTests.mockResolvedValue(testsRead());
    renderPage();
    await openTestsTab();

    const caseTable = await screen.findByRole("table", { name: "Test cases" });
    const suiteTable = screen.getByRole("table", { name: "Test suites" });
    expect(caseTable.compareDocumentPosition(suiteTable)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);

    // The suite header row holds a row header; the body rows do not.
    const rows = within(caseTable)
      .getAllByRole("row")
      .filter((r) => within(r).queryByRole("rowheader") === null)
      .slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[1].textContent)).toEqual(["zeta", "mu", "alpha"]);
    const first = within(rows[0]).getAllByRole("cell").map((c) => c.textContent);
    expect(first).toEqual(["com.example.ZedTest", "zeta", "FAILED", "1s", "expected 1 but was 2"]);
    // A value the record omits renders nothing.
    const second = within(rows[1]).getAllByRole("cell").map((c) => c.textContent);
    expect(second[3]).toBe("");
    expect(second[4]).toBe("");
    // The fixture holds one suite, so one header row names it.
    expect(within(caseTable).getAllByRole("rowheader").map((h) => h.textContent)).toEqual(["unit"]);
  });

  it("puts each suite in a header row and hides the class column when each suite holds one class", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "FAILURE" }));
    mockListRunTests.mockResolvedValue(
      testsRead({
        items: [],
        cases: [
          {
            suite: "github.com/x/alpha",
            className: "github.com/x/alpha",
            name: "TestA",
            status: "FAILED",
            caseKey: "github.com/x/alpha/TestA",
          },
          {
            suite: "github.com/x/beta",
            className: "github.com/x/beta",
            name: "TestB",
            status: "SKIPPED",
            caseKey: "github.com/x/beta/TestB",
          },
        ],
      }),
    );
    renderPage();
    await openTestsTab();

    const caseTable = await screen.findByRole("table", { name: "Test cases" });
    expect(within(caseTable).queryByRole("columnheader", { name: "Class" })).toBeNull();
    expect(within(caseTable).getAllByRole("columnheader")[0].textContent).toBe("Test");
    expect(within(caseTable).getAllByRole("rowheader").map((h) => h.textContent)).toEqual([
      "github.com/x/alpha",
      "github.com/x/beta",
    ]);
  });

  it("puts each tests-tab table in a horizontal scroll container", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "FAILURE" }));
    mockListRunTests.mockResolvedValue(testsRead());
    renderPage();
    await openTestsTab();

    const caseTable = await screen.findByRole("table", { name: "Test cases" });
    expect(caseTable.parentElement?.className).toMatch(/tableScroll/);
    expect(screen.getByRole("table", { name: "Test suites" }).parentElement?.className).toMatch(/tableScroll/);
  });

  it("links each case row to that case's history with the encoded case key", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "FAILURE" }));
    mockListRunTests.mockResolvedValue(testsRead());
    renderPage();
    await openTestsTab();

    const link = await screen.findByRole("link", { name: "zeta" });
    expect(link).toHaveAttribute(
      "href",
      "/pipelines/payments/checkout-api%2Fmain/tests/com.example.ZedTest%2Fzeta",
    );
  });

  it("shows no case table for an empty cases list", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockListRunTests.mockResolvedValue(testsRead({ cases: [] }));
    renderPage();
    await openTestsTab();

    expect(await screen.findByRole("table", { name: "Test suites" })).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "Test cases" })).toBeNull();
  });

  it("names the report path and size, and downloads it through the artifact path", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockListRunTests.mockResolvedValue(
      testsRead({ report: { path: ".varroa/tests.json", sizeBytes: 2048, caseCount: 40, downloadable: true } }),
    );
    mockDownloadRunArtifact.mockResolvedValue(new Blob(["{}"]));
    const { createObjectURL, revokeObjectURL } = URL;
    URL.createObjectURL = vi.fn(() => "blob:report");
    URL.revokeObjectURL = vi.fn();
    // jsdom does not implement a navigation, so the anchor click does nothing.
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    renderPage();
    await openTestsTab();

    expect(await screen.findByText(".varroa/tests.json")).toBeInTheDocument();
    expect(screen.getByText("2048 bytes")).toBeInTheDocument();
    const download = screen.getByRole("button", { name: "Download" });
    expect(download).not.toBeDisabled();
    fireEvent.click(download);
    await vi.waitFor(() => expect(mockDownloadRunArtifact).toHaveBeenCalledWith("run-1", ".varroa/tests.json"));
    await vi.waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:report"));
    expect(click).toHaveBeenCalledTimes(1);
    click.mockRestore();
    URL.createObjectURL = createObjectURL;
    URL.revokeObjectURL = revokeObjectURL;
  });

  it("disables the report download, with an explanation, when the report is local", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockListRunTests.mockResolvedValue(
      testsRead({ report: { path: ".varroa/tests.json", sizeBytes: 10, caseCount: 0, downloadable: false } }),
    );
    renderPage();
    await openTestsTab();

    const download = await screen.findByRole("button", { name: "Download" });
    expect(download).toBeDisabled();
    expect(download).toHaveAttribute("title", "Stored locally by Jenkins; not downloadable");
  });

  it("shows no report line when the read carries no report", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockListRunTests.mockResolvedValue(testsRead());
    renderPage();
    await openTestsTab();

    await screen.findByRole("table", { name: "Test suites" });
    expect(screen.queryByText("Test report")).toBeNull();
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
  });
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

  it("renders the header duration in the format the team table uses", async () => {
    mockGetRun.mockResolvedValue(baseRun({ durationMs: 600_000 }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("10m 0s")).toBeInTheDocument();
    expect(screen.queryByText("600s")).not.toBeInTheDocument();
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

    expect(screen.getByRole("tab", { name: /^Log/ })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: /^Artifacts/ }));
    expect(await screen.findByText("build/out.jar")).toBeInTheDocument();
    const downloadButtons = screen.getAllByRole("button", { name: "Download" });
    expect(downloadButtons[1]).toBeDisabled();
    expect(downloadButtons[0]).not.toBeDisabled();

    fireEvent.click(screen.getByRole("tab", { name: /^Tests/ }));
    expect(await screen.findByText(/failed and skipped cases the run reported, at most 500/)).toBeInTheDocument();
    expect(screen.getByText(/test report holds every case, including each passed case/)).toBeInTheDocument();
    expect(screen.getByText("unit")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: /^Changes/ }));
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
  /** logStreamCall returns the arguments of the last useEventStream call for
   * the run log stream, which the team run stream does not share. */
  function logStreamCall(): Parameters<typeof mockUseEventStream> {
    const calls = mockUseEventStream.mock.calls.filter((args) => args[1] === "run:run-1");
    const args = calls[calls.length - 1];
    if (!args) throw new Error("useEventStream was not called for the run log stream");
    return args;
  }

  function latestResumeUrl(): () => string | null {
    const options = logStreamCall()[3];
    if (!options?.resumeUrl) throw new Error("useEventStream was not called with a resumeUrl option");
    return options.resumeUrl;
  }

  /** logFrame hands the page one log frame the way the hook's onEvent does. */
  function logFrame(frame: { type: string; data: unknown }) {
    const options = logStreamCall()[3];
    if (!options?.onEvent) throw new Error("useEventStream was not called with an onEvent option");
    options.onEvent(frame);
  }

  it("passes useEventStream a resumeUrl with no offset before any log frame arrives", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log");
  });

  it("carries one byte past the last line frame from the last line frame on reconnect", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);

    await act(async () => {
      logFrame({ type: "line", data: { n: 7, off: 512, ts: 1700000000000, text: "building" } });
    });
    await act(async () => {
      repaint();
    });

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log?from=513&fromLine=8");
  });

  it("keeps no resume offset when a gap frame arrives before any line frame", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);

    await act(async () => {
      logFrame({ type: "gap", data: { seq: 4, off: 900 } });
    });
    await act(async () => {
      repaint();
    });

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log");
  });

  it("keeps fromLine and the line watermark across a gap frame before the last line", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);

    await act(async () => {
      logFrame({ type: "line", data: { n: 7, off: 512, ts: 1700000000000, text: "building" } });
      logFrame({ type: "gap", data: { seq: 4, off: 0 } });
    });
    await act(async () => {
      repaint();
    });

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log?from=513&fromLine=8");
  });

  it("keeps the line watermark when a gap frame lies past the last line", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);

    await act(async () => {
      logFrame({ type: "line", data: { n: 7, off: 512, ts: 1700000000000, text: "building" } });
      logFrame({ type: "gap", data: { seq: 4, off: 900 } });
    });
    await act(async () => {
      repaint();
    });

    expect(latestResumeUrl()()).toBe("/api/v1/runs/run-1/log?from=513&fromLine=8");
  });

  it("closes the run log stream on the end frame, so the page stops reconnecting", async () => {
    mockGetRun.mockResolvedValue(baseRun({ status: "RUNNING", result: undefined }));
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);
    expect(logStreamCall()[0]).toBe("/api/v1/runs/run-1/log");

    await act(async () => {
      logFrame({ type: "end", data: { result: "SUCCESS", logComplete: true } });
    });
    await act(async () => {
      repaint();
    });

    // A null base URL keeps the hook closed, so it mints no further ticket and
    // the page never resumes a log that already ended.
    expect(logStreamCall()[0]).toBeNull();
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
      fireEvent.click(screen.getByRole("tab", { name: /^Artifacts/ }));
    });
    expect(document.getElementById("node-stage-s1")).toBeTruthy();

    // A second, unrelated re-render — the same lastEvent, a different tab
    // click — must not wipe the stage the live event already merged.
    await act(async () => {
      fireEvent.click(screen.getByRole("tab", { name: /^Tests/ }));
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
    ["TRIGGER_UNREACHABLE", "The trigger did not reach the controller's Jenkins."],
    ["CLAIM_EXPIRED", "The build did not get a build number in time, so it did not start."],
    ["CLAIM_ABANDONED", "The build got a build number, but it did not start in time."],
    ["FENCED", "The controller that received the trigger no longer hosts this team."],
    ["CROSS_TENANT", "Another team may not start this job."],
    ["INVALID_KEY", "The build request was not valid."],
    ["THROTTLED", "The record store refused the build because of a limit."],
    ["A_FUTURE_CODE", "The controller refused the trigger."],
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
    expect(screen.queryByRole("heading", { name: "Not started" })).not.toBeInTheDocument();
  });

  it("shows the same card on a cold load", async () => {
    mockGetRun.mockResolvedValue(triggerFailedRun("TRIGGER_REJECTED"));
    renderPage();

    expect(await screen.findByText("The controller refused the trigger.")).toBeInTheDocument();
    expect(screen.queryByText("Run not found")).not.toBeInTheDocument();
  });
});

/** LocationProbe writes the router's current search string into the DOM. */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location">{location.search}</div>;
}

function renderWithProbe(entry: InitialEntry) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[entry]}>
        <BuildPage />
        <LocationProbe />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("BuildPage — orientation", () => {
  it("renders the trail from the team to the build", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ tenantFolder: "payments", jobPath: "payments/checkout-api/main" }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    const trail = screen.getByRole("navigation", { name: "Job path" });
    expect(within(trail).getByRole("link", { name: "payments" })).toHaveAttribute("href", "/pipelines/payments");
    expect(within(trail).getByRole("link", { name: "checkout-api" })).toHaveAttribute(
      "href",
      "/pipelines/payments?open=checkout-api",
    );
    expect(within(trail).getByRole("link", { name: "main" })).toHaveAttribute(
      "href",
      "/pipelines/payments/payments%2Fcheckout-api%2Fmain",
    );
    expect(within(trail).getByText("#42")).toHaveAttribute("aria-current", "page");
  });

  it("names the build in the heading and the state first in the document title", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    expect(await screen.findByRole("heading", { level: 1, name: "main · Build #42" })).toBeInTheDocument();
    await waitFor(() => expect(document.title).toBe("Passed · #42 checkout-api/main · payments"));
  });

  it("starts the document title with Running while the build runs", async () => {
    mockGetRun.mockResolvedValue(baseRun({ status: "RUNNING", result: undefined }));
    renderPage();
    await screen.findByText(/Build #42/);
    await waitFor(() => expect(document.title).toMatch(/^Running · #42 /));
  });

  it("opens the tab the URL names", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage("/pipelines/payments/checkout-api%2Fmain/run-1?tab=tests");
    await screen.findByText(/Build #42/);
    expect(screen.getByRole("tab", { name: /Tests/ })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: /Log/ })).toHaveAttribute("aria-selected", "false");
  });

  it("writes the selected tab to the URL", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderWithProbe("/pipelines/payments/checkout-api%2Fmain/run-1");
    await screen.findByText(/Build #42/);
    fireEvent.click(screen.getByRole("tab", { name: /Artifacts/ }));
    expect(screen.getByTestId("location")).toHaveTextContent("?tab=artifacts");
    expect(screen.getByRole("tab", { name: /Artifacts/ })).toHaveAttribute("aria-selected", "true");
  });

  it("falls back to the Log tab and drops an unknown tab value", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderWithProbe("/pipelines/payments/checkout-api%2Fmain/run-1?tab=bogus");
    await screen.findByText(/Build #42/);
    expect(screen.getByRole("tab", { name: /Log/ })).toHaveAttribute("aria-selected", "true");
    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent(/^$/));
  });

  it("shows the trail on a refused trigger card", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        result: "TRIGGER_FAILED",
        buildNumber: undefined,
        durationMs: undefined,
        startedAt: undefined,
        triggerReasonCode: "JOB_NOT_FOUND",
      }),
    );
    renderPage();
    await screen.findByText("The job does not exist on the controller.");
    const trail = screen.getByRole("navigation", { name: "Job path" });
    expect(within(trail).getByText("Not started")).toHaveAttribute("aria-current", "page");
    expect(document.title).toMatch(/^Not started · /);
  });
});

describe("BuildPage — the header's source facts", () => {
  it("renders the branch, the short commit, and the record's own cause on the meta line", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ branch: "main", scmRevision: "abcdef1234567890", cause: "Push" }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    // The trail also names the job "main", so the branch is read from the meta line.
    const meta = screen.getByText("abcdef12").closest("div")!;
    expect(within(meta).getByText("main")).toBeInTheDocument();
    expect(within(meta).getByText("abcdef12")).toBeInTheDocument();
    // The heading carries no cause, so the cause string appears on the meta line alone.
    expect(within(meta).getByText("Push")).toBeInTheDocument();
    expect(screen.queryByText("abcdef1234567890")).not.toBeInTheDocument();
  });

  it("renders the definition path, the libraries, and the pod templates in the Ran with row", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        source: {
          definitionPath: "ci/Jenkinsfile",
          libraries: [{ name: "acme", version: "1.2.3", revision: "abcdef1234567890" }],
          podTemplates: [{ name: "maven", label: "maven", image: "maven:3.9.6" }],
        },
      }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("Ran with")).toBeInTheDocument();
    expect(screen.getByText("ci/Jenkinsfile")).toBeInTheDocument();
    expect(screen.getByText("acme@1.2.3 abcdef12")).toBeInTheDocument();
    expect(screen.getByText("maven (maven:3.9.6)")).toBeInTheDocument();
  });

  it("renders the row's values alone when the record carries no version and no revision", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ source: { libraries: [{ name: "acme" }], podTemplates: [{ name: "maven" }] } }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("Ran with")).toBeInTheDocument();
    expect(screen.getByText("acme")).toBeInTheDocument();
    expect(screen.getByText("maven")).toBeInTheDocument();
  });

  it("renders no Ran with row for a record with no source object", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByText("Ran with")).not.toBeInTheDocument();
  });

  it("renders no Ran with row for an empty source object", async () => {
    mockGetRun.mockResolvedValue(baseRun({ source: {} }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByText("Ran with")).not.toBeInTheDocument();
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

  it("refetches the record for a lock event of this run, which the notifier can send in place of a stage event", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);
    const before = mockGetRun.mock.calls.length;

    runStream({ type: "lock", data: notification({ kind: "lock", lock_name: "payments/deploy", state: "HELD" }) });
    await act(async () => {
      repaint();
    });

    expect(mockGetRun.mock.calls.length).toBeGreaterThan(before);
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

describe("BuildPage — run summary and failure digest", () => {
  it("renders the summary row when the record carries a summary", async () => {
    mockGetRun.mockResolvedValue(baseRun({ summary: "flaky on the first attempt" }));
    renderPage();

    expect(await screen.findByText("flaky on the first attempt")).toBeInTheDocument();
    expect(screen.getByText("Summary")).toBeInTheDocument();
  });

  it("renders no summary row when the record carries no summary", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByText("Summary")).toBeNull();
  });

  it("renders every part of a full digest", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        result: "FAILURE",
        digest: {
          stage: "Integration Test",
          error: "three tests failed",
          failedTests: { failed: 2, skipped: 1, total: 10 },
          logTail: "Traceback\n  at job.groovy:12",
        },
      }),
    );
    renderPage();

    expect(await screen.findByText("Integration Test")).toBeInTheDocument();
    expect(screen.getByText(/: three tests failed/)).toBeInTheDocument();
    expect(screen.getByText("2 failed, 1 skipped of 10")).toBeInTheDocument();
    expect(screen.getByText(/at job\.groovy:12/).tagName).toBe("PRE");
  });

  it("names the digest's failed cases beside the counts, each linked to its history", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        result: "FAILURE",
        digest: {
          failedTests: {
            failed: 2,
            total: 10,
            cases: [
              { suite: "unit", className: "com.example.FooTest", name: "bar", caseKey: "com.example.FooTest/bar" },
              { suite: "unit", className: "com.example.FooTest", name: "baz", caseKey: "com.example.FooTest/baz" },
            ],
          },
        },
      }),
    );
    renderPage();

    expect(await screen.findByText("2 failed of 10")).toBeInTheDocument();
    const list = screen.getByRole("list", { name: "Failed test cases" });
    const links = within(list).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["com.example.FooTest › bar", "com.example.FooTest › baz"]);
    expect(links[0]).toHaveAttribute("href", "/pipelines/payments/checkout-api%2Fmain/tests/com.example.FooTest%2Fbar");
  });

  it("shows the counts alone when the digest names no case", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "FAILURE", digest: { failedTests: { failed: 3 } } }));
    renderPage();

    expect(await screen.findByText("3 failed")).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Failed test cases" })).toBeNull();
  });

  it("renders the digest card for a log tail alone, with no stage line and no counts", async () => {
    mockGetRun.mockResolvedValue(baseRun({ digest: { logTail: "boom" } }));
    renderPage();

    expect(await screen.findByText("Digest")).toBeInTheDocument();
    const tail = screen.getByText("boom");
    expect(tail.tagName).toBe("PRE");
    // The card body holds the tail alone: no stage line and no test counts.
    expect(tail.parentElement?.textContent).toBe("boom");
    expect(screen.queryByText(/\bfailed\b/)).toBeNull();
    expect(screen.queryByText(/skipped/)).toBeNull();
  });

  it("renders no digest card when the record carries no digest", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByText("Digest")).toBeNull();
  });

  it("renders a lone failed count with no skipped count and no total", async () => {
    mockGetRun.mockResolvedValue(baseRun({ result: "FAILURE", digest: { failedTests: { failed: 1 } } }));
    renderPage();

    expect(await screen.findByText("1 failed")).toBeInTheDocument();
    expect(screen.queryByText(/skipped/)).toBeNull();
    expect(screen.queryByText(/of \d/)).toBeNull();
  });

  it("shows the interrupted banner, then the digest card, then the pipeline card", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({ result: "INTERRUPTED", interruptedReason: "node lost", digest: { stage: "Deploy" } }),
    );
    renderPage();

    const banner = await screen.findByRole("alert");
    const digestTitle = screen.getByText("Digest");
    const pipelineTitle = screen.getByText("Pipeline");

    expect(banner.compareDocumentPosition(digestTitle)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(digestTitle.compareDocumentPosition(pipelineTitle)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  });
});

describe("BuildPage — pending inputs", () => {
  /** pendingInput builds one input step on top of baseRun's run. */
  function pendingInput(overrides: Partial<RunInput> = {}): RunInput {
    return {
      id: "in-1",
      message: "Approve the release?",
      ok: "Ship it",
      parameters: [
        {
          name: "ENV",
          type: "ChoiceParameterDefinition",
          defaultValue: "prod",
          choices: ["prod", "staging"],
        },
      ],
      ...overrides,
    };
  }

  /** grantTenantPipelinesRead makes usePermissions report a payments read
   * grant and no write grant. */
  function grantTenantPipelinesRead() {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "payments", jobs: [], capabilities: { pipelines: { read: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
  }

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

  /** flush settles the request the page started. */
  async function flush() {
    await act(async () => {
      await Promise.resolve();
    });
  }

  it("renders the pending input card under the digest card and above the pipeline card", async () => {
    mockGetRun.mockResolvedValue(baseRun({ digest: { stage: "Deploy" }, inputs: [pendingInput()] }));
    renderPage();

    const digestTitle = await screen.findByText("Digest");
    const inputTitle = screen.getByText("Pending input");
    const pipelineTitle = screen.getByText("Pipeline");

    expect(digestTitle.compareDocumentPosition(inputTitle)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(inputTitle.compareDocumentPosition(pipelineTitle)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  });

  it("renders the message, the parameter control, the step's own proceed label, and the abort control", async () => {
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("Approve the release?")).toBeInTheDocument();
    expect(screen.getByLabelText("ENV")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Ship it" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Abort" })).toBeInTheDocument();
  });

  it("renders a fallback message for an input that declares none", async () => {
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput({ message: undefined })] }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("This build waits for an answer.")).toBeInTheDocument();
  });

  it("renders a declared choice list as a select that starts at the declared default", async () => {
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);

    const control = screen.getByLabelText("ENV") as HTMLSelectElement;
    expect(control.tagName).toBe("SELECT");
    expect(control.value).toBe("prod");
    expect(screen.getByRole("option", { name: "staging" })).toBeInTheDocument();
  });

  it("renders a file parameter as a disabled control with an explanation", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        inputs: [pendingInput({ parameters: [{ name: "BUNDLE", type: "FileParameterDefinition" }] })],
      }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByLabelText("BUNDLE")).toBeDisabled();
    expect(
      screen.getByText("A file parameter cannot be set from this page. The step uses its own value."),
    ).toBeInTheDocument();
  });

  it("renders no form for an input that declares no parameter, and sends an empty body", async () => {
    mockProceedRunInput.mockResolvedValue({ state: "requested" });
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput({ parameters: [] })] }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByLabelText("ENV")).toBeNull();
    // The empty list renders no control at all.
    const card = screen.getByText("Pending input").parentElement?.parentElement;
    expect(card!.querySelectorAll("input, select, textarea")).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Ship it" }));
    await flush();

    expect(mockProceedRunInput).toHaveBeenCalledWith("run-1", "in-1", {});
  });

  it("labels the proceed control Proceed when the step declares no label", async () => {
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput({ ok: undefined })] }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByRole("button", { name: "Proceed" })).toBeInTheDocument();
  });

  it("names the declared submitter and renders no control for the submitter parameter", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        inputs: [pendingInput({ submitter: "payments/release-managers", submitterParameter: "APPROVER" })],
      }),
    );
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("Only payments/release-managers may answer this input.")).toBeInTheDocument();
    expect(screen.queryByLabelText("APPROVER")).toBeNull();
  });

  it("renders a read-only caller the card, no answer control, and every control disabled", async () => {
    grantTenantPipelinesRead();
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.getByText("Approve the release?")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Ship it" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Abort" })).toBeNull();
    expect(screen.getByLabelText("ENV")).toBeDisabled();
  });

  it("refetches the record for an input frame of this run, which the page subscribes to", async () => {
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    const { repaint } = renderPage();
    await screen.findByText(/Build #42/);
    const before = mockGetRun.mock.calls.length;

    runStream({ type: "input", data: notification({ kind: "input" }) });
    await act(async () => {
      repaint();
    });

    expect(mockGetRun.mock.calls.length).toBeGreaterThan(before);
    const teamStream = mockUseEventStream.mock.calls.find((call) => call[1] === "pipelines:payments");
    expect(teamStream?.[2]).toContain("input");
  });

  it("sends no value for an untouched control, so the plugin applies the declared default", async () => {
    mockProceedRunInput.mockResolvedValue({ state: "requested" });
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);

    fireEvent.click(screen.getByRole("button", { name: "Ship it" }));
    await flush();

    expect(mockProceedRunInput).toHaveBeenCalledWith("run-1", "in-1", {});
  });

  it("shows the requested toast, refetches the record, sends the edited body, and keeps the entry", async () => {
    mockProceedRunInput.mockResolvedValue({ state: "requested" });
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);
    const before = mockGetRun.mock.calls.length;

    fireEvent.change(screen.getByLabelText("ENV"), { target: { value: "staging" } });
    fireEvent.click(screen.getByRole("button", { name: "Ship it" }));
    await flush();

    expect(mockProceedRunInput).toHaveBeenCalledWith("run-1", "in-1", { ENV: "staging" });
    expect(mockToast).toHaveBeenCalledWith("Proceed requested");
    expect(mockGetRun.mock.calls.length).toBeGreaterThan(before);
    // The record still carries the input, so the entry stands.
    expect(screen.getByText("Approve the release?")).toBeInTheDocument();
  });

  it("shows the abort requested toast for the abort control", async () => {
    mockAbortRunInput.mockResolvedValue({ state: "requested" });
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);

    fireEvent.click(screen.getByRole("button", { name: "Abort" }));
    await flush();

    expect(mockAbortRunInput).toHaveBeenCalledWith("run-1", "in-1");
    expect(mockToast).toHaveBeenCalledWith("Abort requested");
  });

  it("disables the controls of the entry while a request is in flight", async () => {
    mockProceedRunInput.mockReturnValue(new Promise(() => {}));
    mockGetRun.mockResolvedValue(baseRun({ inputs: [pendingInput()] }));
    renderPage();
    await screen.findByText(/Build #42/);

    fireEvent.click(screen.getByRole("button", { name: "Ship it" }));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(mockProceedRunInput).toHaveBeenCalledTimes(1);
    // The pending request disables the whole entry: both answer controls and
    // the parameter control.
    const proceed = screen.getByRole("button", { name: "Ship it" });
    expect(proceed.outerHTML).toContain("disabled");
    expect(screen.getByRole("button", { name: "Abort" }).outerHTML).toContain("disabled");
    expect(screen.getByLabelText("ENV")).toBeDisabled();
  });

  it("renders no pending input card for a run that holds no input", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByText("Pending input")).toBeNull();
  });
});

describe("BuildPage — replay", () => {
  function scriptRead(overrides: Partial<RunScript> = {}): RunScript {
    return {
      script: "pipeline { agent any }",
      loadedScripts: {},
      definitionKind: "script",
      replayable: true,
      reason: "",
      ...overrides,
    };
  }

  function grantTenantPipelinesRead() {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [],
        tenants: [{ tenant: "payments", jobs: [], capabilities: { pipelines: { read: true } } }],
      },
    } as unknown as ReturnType<typeof usePermissions>);
  }

  it("shows the Replay control for a finished run that replays", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockGetRunScript.mockResolvedValue(scriptRead());
    renderPage();

    expect(await screen.findByRole("button", { name: /Replay/ })).toBeInTheDocument();
    expect(mockGetRunScript).toHaveBeenCalledWith("run-1");
    expect(screen.getByRole("button", { name: /Re-run/ })).toBeInTheDocument();
  });

  it("shows no Replay control and reads no script without the write grant", async () => {
    grantTenantPipelinesRead();
    mockGetRun.mockResolvedValue(baseRun());
    mockGetRunScript.mockResolvedValue(scriptRead());
    renderPage();
    await screen.findByText(/Build #42/);

    expect(screen.queryByRole("button", { name: /Replay/ })).toBeNull();
    expect(mockGetRunScript).not.toHaveBeenCalled();
  });

  it("shows no Replay control and no reason for a run that does not replay", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockGetRunScript.mockResolvedValue(scriptRead({ script: "", replayable: false, reason: "The run used a masked parameter." }));
    renderPage();
    await screen.findByText(/Build #42/);
    await waitFor(() => expect(mockGetRunScript).toHaveBeenCalled());

    expect(screen.queryByRole("button", { name: /Replay/ })).toBeNull();
    expect(screen.queryByText("The run used a masked parameter.")).toBeNull();
  });

  it("reads no script for a run that has not ended", async () => {
    mockGetRun.mockResolvedValue(baseRun({ status: "RUNNING", result: undefined }));
    renderPage();
    await screen.findByText(/Build #42/);

    expect(mockGetRunScript).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /Replay/ })).toBeNull();
  });

  it("submits the edited script and opens the new run in the requested state", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockGetRunScript.mockResolvedValue(scriptRead());
    mockReplayRun.mockResolvedValue({ runId: "run-9", intentKey: "replay:run-1:run-9", state: "requested" });
    editorText = "pipeline { agent none }";
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Replay/ }));
    const drawer = screen.getByRole("dialog", { name: "Replay this build" });
    fireEvent.click(within(drawer).getByRole("button", { name: "Replay" }));

    await waitFor(() =>
      expect(mockNavigate).toHaveBeenCalledWith("/pipelines/payments/checkout-api%2Fmain/run-9", {
        state: { requested: true },
      }),
    );
    expect(mockReplayRun).toHaveBeenCalledWith("run-1", "pipeline { agent none }");
    expect(mockToast).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog", { name: "Replay this build" })).toBeNull();
  });

  it("closes the drawer without a request", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    mockGetRunScript.mockResolvedValue(scriptRead());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Replay/ }));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));

    expect(screen.queryByRole("dialog", { name: "Replay this build" })).toBeNull();
    expect(mockReplayRun).not.toHaveBeenCalled();
  });
});

describe("BuildPage — refused command notice", () => {
  it("shows the newest refused cancel with its status, actor, and error", async () => {
    mockGetRun.mockResolvedValue(
      baseRun({
        commandRefusal: {
          command: "cancel",
          actor: "alice",
          httpStatus: 404,
          error: "Not Found: no build carries run id run-1",
          refusedAt: "2026-09-29T12:03:00Z",
        },
      }),
    );
    renderPage();
    await screen.findByText(/Build #42/);
    const notice = screen.getByText(
      /^Cancel refused \(HTTP 404\) by alice at .*: Not Found: no build carries run id run-1$/,
    );
    expect(notice).toHaveAttribute("role", "status");
  });

  it("shows no notice when the run holds no refusal", async () => {
    mockGetRun.mockResolvedValue(baseRun());
    renderPage();
    await screen.findByText(/Build #42/);
    expect(screen.queryByText(/refused/)).not.toBeInTheDocument();
  });
});
