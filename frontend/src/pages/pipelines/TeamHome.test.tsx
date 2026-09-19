import fs from "fs";
import path from "path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Routes, Route, useLocation, useNavigate } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import { createPipelineSummary, createRunningRun } from "../../test/factories";
import TeamHome from "./TeamHome";

const mockListPipelines = vi.fn();
const mockListRunners = vi.fn();
const mockGetTeamCell = vi.fn();
vi.mock("../../api/client", () => ({
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
  listRunners: (...args: unknown[]) => mockListRunners(...args),
  getTeamCell: (...args: unknown[]) => mockGetTeamCell(...args),
}));

/** LocationSpy publishes the current search string, so a test can read the
 * URL the page wrote. */
function LocationSpy() {
  const location = useLocation();
  return <div data-testid="search">{location.search}</div>;
}

function renderPage(route = "/pipelines/acme") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <LocationSpy />
        <Routes>
          <Route path="/pipelines/:team" element={<TeamHome />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
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
          <Route path="/pipelines/:team" element={<TeamHome />} />
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

describe("TeamHome", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTeamCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("shows a loading state before the response arrives", () => {
    mockListPipelines.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByText(/Loading pipelines/)).toBeInTheDocument();
  });

  it("shows the empty state for a team with no running builds and no pipelines", async () => {
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Nothing is running.")).toBeInTheDocument();
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
  });

  it("renders a never-run job with an empty last build and no health glyph", async () => {
    mockListPipelines.mockResolvedValue({
      items: [{ jobPath: "payments/checkout-api/main", health: "unknown" }],
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
      items: [{ jobPath: "payments/checkout-api/main", health: "rain" }],
      runningNow: [],
      truncated: false,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("img", { name: /Rain/ })).toBeInTheDocument();
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
});

describe("TeamHome — the folder tree", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTeamCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
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

describe("TeamHome — the fold state", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTeamCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
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

describe("TeamHome — Runners section", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTeamCell.mockReset();
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("shows the recovery state for a team with no registered runners", async () => {
    mockListRunners.mockResolvedValue([]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("No runners registered for this team yet.")).toBeInTheDocument();
    });
  });

  it("renders a runner row with its launcher, labels, cell, and connection state", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "win-1",
        spec: { team: "acme", launcher: "ssh", labels: ["windows", "msbuild"], remoteFS: "C:\\J" },
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

  it("renders an unconnected runner with dashes for its unset fields", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "agent-1",
        spec: { team: "acme", launcher: "inbound", remoteFS: "/home/jenkins" },
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
        spec: { team: "acme", launcher: "inbound", remoteFS: "/home/jenkins" },
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

describe("TeamHome — the cell card", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTeamCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/checkout-api"]));
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  });

  it("puts the pill in the page head after the title and opens the card from it", async () => {
    const user = userEvent.setup();
    mockGetTeamCell.mockResolvedValue({
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

  it("heads the page with an h1 and the card with an h2", async () => {
    const user = userEvent.setup();
    mockGetTeamCell.mockResolvedValue({ state: "placed", cell: "cell-a", canOpenCell: false });
    renderPage("/pipelines/payments");

    await user.click(await screen.findByRole("button", { name: "Placed · cell-a" }));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("payments");
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Your cell");
  });

  it("keeps the pipelines table when the cell read fails", async () => {
    mockGetTeamCell.mockRejectedValue(new Error("boom"));
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    await waitFor(() => expect(screen.queryByText("Your cell")).not.toBeInTheDocument());
  });

  it("refreshes the cell read on its own interval and leaves the other reads alone", async () => {
    mockGetTeamCell.mockClear();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderPage("/pipelines/payments");
      await waitFor(() => expect(mockGetTeamCell).toHaveBeenCalledTimes(1));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });

      expect(mockGetTeamCell).toHaveBeenCalledTimes(3);
      expect(mockListPipelines).toHaveBeenCalledTimes(1);
      expect(mockListRunners).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("TeamHome — the cell pill", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockGetTeamCell.mockReset();
    mockListRunners.mockResolvedValue([]);
    mockListPipelines.mockResolvedValue(pipelineList("payments", ["payments/checkout-api"]));
  });

  /** cellPill finds the pill by the region it controls, so a page that
   * renders none answers null. */
  function cellPill(): Element | null {
    return document.querySelector('[aria-controls="team-cell-disclosure"]');
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
      mockGetTeamCell.mockResolvedValue(
        cell ? { state, cell, canOpenCell: false } : { state, canOpenCell: false },
      );
      const { unmount } = renderPage("/pipelines/payments");

      const pill = await screen.findByRole("button", { name });
      expect(pill.textContent).toBe(name);
      unmount();
    }
  });

  it("renders no pill for a team on its own controller", async () => {
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    expect(cellPill()).toBeNull();
  });

  it("renders no pill while the cell read is pending", async () => {
    mockGetTeamCell.mockReturnValue(new Promise(() => {}));
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    expect(cellPill()).toBeNull();
  });

  it("renders no pill when the first cell read fails", async () => {
    mockGetTeamCell.mockRejectedValue(new Error("boom"));
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("checkout-api")).toBeInTheDocument());
    expect(cellPill()).toBeNull();
  });

  it("hides the pill when a successful read is followed by a failed refetch", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      mockGetTeamCell.mockResolvedValueOnce({ state: "placed", cell: "cell-a", canOpenCell: false });
      mockGetTeamCell.mockRejectedValue(new Error("boom"));
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
    mockGetTeamCell.mockResolvedValue({ state: "placed", cell: "cell-a", canOpenCell: false });
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
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    renderPage("/pipelines/payments");

    await waitFor(() => expect(screen.getByText("Queue wait")).toBeInTheDocument());
    const withWait = screen.getByRole("link", { name: "with-wait" }).closest("tr") as HTMLTableRowElement;
    const withoutWait = screen.getByRole("link", { name: "without-wait" }).closest("tr") as HTMLTableRowElement;

    expect(withWait.querySelectorAll("td")[5].textContent).toBe("30s");
    expect(withoutWait.querySelectorAll("td")[5].textContent).toBe("—");
  });
});
