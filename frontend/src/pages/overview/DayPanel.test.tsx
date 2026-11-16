import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DayPanel } from "./DayPanel";
import { renderWithProviders } from "../../test/render-utils";
import { createControllerListItem } from "../../test/factories";
import { fleetRun, hourly, overview, totals } from "../../test/overviewFactories";
import { ApiError } from "../../hooks/useApi";

const mocks = vi.hoisted(() => ({ getPipelinesOverview: vi.fn() }));
vi.mock("../../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/client")>()),
  ...mocks,
}));

const mockUseControllers = vi.fn();
vi.mock("../../hooks/useControllers", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../hooks/useControllers")>()),
  useControllers: (enabled?: boolean) => mockUseControllers(enabled),
}));

vi.mock("../../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../hooks/useApi")>()),
  bffFetch: vi.fn().mockResolvedValue({ items: [] }),
}));

function controllersReturn(data: unknown[] | undefined, extra: Record<string, unknown> = {}) {
  return { data, isPending: false, isError: false, error: null, refetch: vi.fn(), ...extra };
}

/** exactText matches the innermost element whose full text reads the given string. */
function exactText(text: string) {
  return (_: string, el: Element | null) =>
    el?.textContent === text && ![...el.children].some((c) => c.textContent === text);
}

function renderDay(props: { canPipelines?: boolean; canControllers?: boolean } = {}) {
  return renderWithProviders(<DayPanel canPipelines={props.canPipelines ?? true} canControllers={props.canControllers ?? true} />);
}

describe("DayPanel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseControllers.mockReturnValue(controllersReturn([]));
    mocks.getPipelinesOverview.mockResolvedValue(overview());
  });

  it("shows the totals line and 24 bars", async () => {
    renderDay({ canControllers: false });
    expect(await screen.findByText(exactText("12 runs · 10 passed · 2 failed · median 1m"))).toBeInTheDocument();
    const bars = screen.getByRole("img", { name: /Runs per hour, last 24 hours/ });
    expect(bars.children).toHaveLength(24);
  });

  it("marks an hour with a failure and names it in the label", async () => {
    mocks.getPipelinesOverview.mockResolvedValue(overview({ hourly: hourly([5]) }));
    renderDay({ canControllers: false });
    const bars = await screen.findByRole("img", { name: /Failures at \d\d:\d\d\./ });
    const failed = Array.from(bars.children).filter((b) => b.getAttribute("data-failed") === "true");
    expect(failed).toHaveLength(1);
  });

  it("says No runs in the last 24 hours", async () => {
    mocks.getPipelinesOverview.mockResolvedValue(overview({ totals: totals({ runs24h: 0, passed: 0, failed: 0 }), hourly: hourly([], 0) }));
    renderDay({ canControllers: false });
    expect(await screen.findByText("No runs in the last 24 hours.")).toBeInTheDocument();
  });

  it("lists a running run with its stage and the queue", async () => {
    const running = {
      run: fleetRun({ runId: "r1", status: "RUNNING", result: undefined, buildNumber: 341, tenant: "payments", jobPath: "build", startedAt: new Date(Date.now() - 120_000).toISOString() }),
      stage: "Test",
    };
    mocks.getPipelinesOverview.mockResolvedValue(overview({ running: [running], totals: totals({ queued: 2 }) }));
    renderDay({ canControllers: false });
    expect(await screen.findByText("payments / build #341")).toBeInTheDocument();
    expect(screen.getByText(/Test stage/)).toBeInTheDocument();
    expect(screen.getByText(/2 queued · no scheduled run/)).toBeInTheDocument();
  });

  it("names the next scheduled run", async () => {
    mocks.getPipelinesOverview.mockResolvedValue(
      overview({
        nextScheduled: { tenant: "smoke-a", tenantFolder: "", cluster: "core", jobPath: "cron-probe", at: "2026-11-14T12:01:00Z" },
      }),
    );
    renderDay({ canControllers: false });
    expect(await screen.findByText(/next scheduled: smoke-a \/ cron-probe at /)).toBeInTheDocument();
  });

  it("says Nothing is running and no scheduled run", async () => {
    renderDay({ canControllers: false });
    expect(await screen.findByText("Nothing is running.")).toBeInTheDocument();
    expect(screen.getByText("0 queued · no scheduled run")).toBeInTheDocument();
  });

  it("shows the Jenkins counts and the phase bar", async () => {
    mockUseControllers.mockReturnValue(
      controllersReturn([
        createControllerListItem({ miteConnected: true, phase: "Connected" }),
        createControllerListItem({ miteConnected: true, phase: "Connected" }),
        createControllerListItem({ miteConnected: false, phase: "Hibernated" }),
      ]),
    );
    renderDay({ canPipelines: false });
    expect(await screen.findByText(exactText("2 of 3 connected · 1 asleep"))).toBeInTheDocument();
    expect(screen.getByRole("img", { name: "Phase distribution: 2 Connected, 1 Hibernated" })).toBeInTheDocument();
  });

  it("explains a disabled record store", async () => {
    mocks.getPipelinesOverview.mockRejectedValue(new ApiError(503, "off", { code: "record_store_disabled" }));
    renderDay({ canControllers: false });
    expect(await screen.findByText("Pipeline data is off")).toBeInTheDocument();
  });

  it("explains an unreachable record store and Retry reads again", async () => {
    mocks.getPipelinesOverview.mockRejectedValue(new ApiError(503, "down", { code: "record_store_unreachable" }));
    renderDay({ canControllers: false });
    expect(await screen.findByText("Pipeline data is unreachable")).toBeInTheDocument();
    const calls = mocks.getPipelinesOverview.mock.calls.length;
    mocks.getPipelinesOverview.mockResolvedValue(overview());
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(mocks.getPipelinesOverview.mock.calls.length).toBeGreaterThan(calls));
  });

  it("shows Retry when the overview fails", async () => {
    mocks.getPipelinesOverview.mockRejectedValue(new Error("boom"));
    renderDay({ canControllers: false });
    expect(await screen.findByText("Unable to load pipelines")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("a controllers error keeps the pipelines block", async () => {
    mockUseControllers.mockReturnValue(controllersReturn(undefined, { isError: true, error: new Error("x") }));
    renderDay();
    expect(await screen.findByText(exactText("12 runs · 10 passed · 2 failed · median 1m"))).toBeInTheDocument();
    expect(screen.getByText(/Varroa could not load the controllers\./)).toBeInTheDocument();
  });

  it("a controllers-only user sees only the Jenkins row", async () => {
    mockUseControllers.mockReturnValue(controllersReturn([createControllerListItem({ miteConnected: true, phase: "Connected" })]));
    renderDay({ canPipelines: false });
    const row = (await screen.findByText(exactText("1 of 1 connected"))).closest("div") as HTMLElement;
    expect(within(row.parentElement as HTMLElement).getByRole("link", { name: "Controllers" })).toBeInTheDocument();
    expect(mocks.getPipelinesOverview).not.toHaveBeenCalled();
    expect(screen.queryByText(/Runs per hour/)).toBeNull();
  });
  it("says 1 run for one run", async () => {
    mocks.getPipelinesOverview.mockResolvedValue(
      overview({ totals: totals({ runs24h: 1, passed: 1, failed: 0, finished: 1 }) }),
    );
    renderDay({ canControllers: false });
    expect(await screen.findByText(exactText("1 run · 1 passed · 0 failed · median 1m"))).toBeInTheDocument();
  });
});
