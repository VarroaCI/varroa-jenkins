import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { createTestQueryClient } from "../../test/render-utils";
import { ToastProvider } from "../../components/Toast";
import type { FleetRunPage, FleetRunRow } from "../../types";
import { DEFAULT_FEED_QUERY, parseFeedQuery, type FeedQuery } from "./feedQuery";
import { RunFeed, type FeedLive } from "./RunFeed";

const mockListFleetRuns = vi.fn();
const mockRerunRun = vi.fn();
const mockCancelRun = vi.fn();

/** lastListArgs answers the query of the most recent feed read. */
function lastListArgs(): Record<string, unknown> {
  const calls = mockListFleetRuns.mock.calls;
  return calls[calls.length - 1]?.[0] as Record<string, unknown>;
}

vi.mock("../../api/client", () => ({
  listFleetRuns: (...args: unknown[]) => mockListFleetRuns(...args),
  rerunRun: (...args: unknown[]) => mockRerunRun(...args),
  cancelRun: (...args: unknown[]) => mockCancelRun(...args),
}));

function run(over: Partial<FleetRunRow> = {}): FleetRunRow {
  return {
    runId: "r1",
    status: "COMPLETED",
    result: "FAILURE",
    cluster: "core",
    tenant: "pay",
    tenantFolder: "pay",
    jobPath: "pay/api",
    buildNumber: 7,
    actor: "nate",
    branch: "main",
    changeSummary: "Fix the parser",
    trigger: { kind: "push", label: "Push" },
    pendingInput: false,
    canWrite: true,
    createdAt: new Date().toISOString(),
    ...over,
  };
}

function page(items: FleetRunRow[], nextCursor?: string): FleetRunPage {
  return { items, nextCursor };
}

const idleLive: FeedLive = { updatedAt: Date.now(), newRuns: 0, onShowNew: () => {} };

function Harness({ initial, live = idleLive }: { initial: FeedQuery; live?: FeedLive }) {
  const [query, setQuery] = useState<FeedQuery>(initial);
  return (
    <RunFeed query={query} onQuery={setQuery} tenants={["pay", "ops"]} localCluster="core" live={live} />
  );
}

function renderFeed(initial: FeedQuery = DEFAULT_FEED_QUERY, live?: FeedLive) {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ToastProvider>
          <Harness initial={initial} live={live} />
        </ToastProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("RunFeed", () => {
  beforeEach(() => {
    mockListFleetRuns.mockReset();
    mockRerunRun.mockReset();
    mockCancelRun.mockReset();
    mockListFleetRuns.mockResolvedValue(page([run()]));
  });

  it("sends the URL query to listFleetRuns", async () => {
    renderFeed(parseFeedQuery(new URLSearchParams("result=FAILURE&sort=duration&order=asc")));
    await waitFor(() => {
      expect(mockListFleetRuns).toHaveBeenCalledWith(
        expect.objectContaining({ result: "FAILURE", sort: "duration", order: "asc" }),
      );
    });
  });

  it("a header click sorts and drops the cursor", async () => {
    const user = userEvent.setup();
    mockListFleetRuns.mockResolvedValueOnce(page([run()], "c1"));
    renderFeed();
    await screen.findByRole("button", { name: "Show older runs" });
    await user.click(screen.getByRole("button", { name: "Show older runs" }));
    await waitFor(() => expect(mockListFleetRuns).toHaveBeenLastCalledWith(expect.objectContaining({ cursor: "c1" })));
    await user.click(screen.getByRole("button", { name: "Duration" }));
    await waitFor(() => {
      const last = lastListArgs();
      expect(last).toEqual(expect.objectContaining({ sort: "duration", order: "desc" }));
      expect(last.cursor).toBeUndefined();
    });
  });

  it("Show older runs passes nextCursor", async () => {
    const user = userEvent.setup();
    mockListFleetRuns.mockResolvedValueOnce(page([run()], "next-1"));
    mockListFleetRuns.mockResolvedValueOnce(page([run({ runId: "r2" })]));
    renderFeed();
    await user.click(await screen.findByRole("button", { name: "Show older runs" }));
    await waitFor(() => {
      expect(mockListFleetRuns).toHaveBeenLastCalledWith(expect.objectContaining({ cursor: "next-1" }));
    });
  });

  it("a feed error shows Retry", async () => {
    mockListFleetRuns.mockRejectedValue(new Error("boom"));
    renderFeed();
    expect(await screen.findByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("the Status select writes result for Failure", async () => {
    const user = userEvent.setup();
    renderFeed();
    await screen.findByText("Fix the parser");
    await user.selectOptions(screen.getByLabelText("Status"), "FAILURE");
    await waitFor(() => {
      expect(mockListFleetRuns).toHaveBeenLastCalledWith(expect.objectContaining({ result: "FAILURE" }));
    });
  });

  it("the banner shows the new run count", async () => {
    const user = userEvent.setup();
    const onShowNew = vi.fn();
    renderFeed(DEFAULT_FEED_QUERY, { updatedAt: Date.now(), newRuns: 3, onShowNew });
    await user.click(await screen.findByRole("button", { name: "3 new runs" }));
    expect(onShowNew).toHaveBeenCalledTimes(1);
  });

  it("shows No runs yet with the default filters and no rows", async () => {
    mockListFleetRuns.mockResolvedValue(page([]));
    renderFeed();
    expect(await screen.findByText("No runs yet")).toBeInTheDocument();
  });

  it("shows No runs match these filters under filters and Clear resets them", async () => {
    const user = userEvent.setup();
    mockListFleetRuns.mockResolvedValue(page([]));
    renderFeed(parseFeedQuery(new URLSearchParams("result=FAILURE")));
    const empty = (await screen.findByText("No runs match these filters.")).parentElement as HTMLElement;
    await user.click(within(empty).getByRole("button", { name: "Clear" }));
    await waitFor(() => {
      expect(lastListArgs().result).toBeFalsy();
    });
  });

  it("Re-run calls the client and shows a toast", async () => {
    const user = userEvent.setup();
    mockRerunRun.mockResolvedValue({ runId: "r9" });
    mockListFleetRuns.mockResolvedValue(page([run({ result: "SUCCESS" })]));
    renderFeed();
    await user.click(await screen.findByRole("button", { name: /Actions for/ }));
    await user.click(screen.getByRole("menuitem", { name: "Re-run" }));
    await waitFor(() => expect(mockRerunRun).toHaveBeenCalledWith("r1"));
    expect(await screen.findByText(/Re-run requested/)).toBeInTheDocument();
  });

  it("the table headers are sortable and Branch is plain", async () => {
    renderFeed();
    await screen.findByText("Fix the parser");
    const table = screen.getByRole("table");
    expect(within(table).getByRole("columnheader", { name: /Duration/ })).toHaveAttribute("aria-sort", "none");
    expect(within(table).getByRole("columnheader", { name: "Branch" })).not.toHaveAttribute("aria-sort");
  });
});

/** cronRun builds one finished cron run of pay/api, numbered n, with no commit subject. */
function cronRun(n: number): FleetRunRow {
  return run({
    runId: `cron-${n}`,
    buildNumber: n,
    changeSummary: undefined,
    actor: "timer",
    trigger: { kind: "cron", label: "Cron" },
    branch: undefined,
    canWrite: false,
  });
}

describe("RunFeed folds", () => {
  beforeEach(() => {
    mockListFleetRuns.mockReset();
  });

  it("folds consecutive cron runs of one job, and expands them on a click", async () => {
    const user = userEvent.setup();
    mockListFleetRuns.mockResolvedValue(page([cronRun(3), cronRun(2), cronRun(1)]));
    renderFeed();
    const toggle = await screen.findByRole("button", { name: "api × 3" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("link", { name: "api #3" })).not.toBeInTheDocument();
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("link", { name: "api #3" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "api #1" })).toBeInTheDocument();
  });

  it("keeps one run as a row, with no fold button", async () => {
    mockListFleetRuns.mockResolvedValue(page([cronRun(1)]));
    renderFeed();
    expect(await screen.findByRole("link", { name: "api #1" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /×/ })).not.toBeInTheDocument();
  });

  it("lists every run without a fold under a job filter", async () => {
    mockListFleetRuns.mockResolvedValue(page([cronRun(3), cronRun(2), cronRun(1)]));
    renderFeed(parseFeedQuery(new URLSearchParams("job=pay/api")));
    expect(await screen.findByRole("link", { name: "api #3" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /×/ })).not.toBeInTheDocument();
  });

  it("lists every run without a fold under the duration sort", async () => {
    mockListFleetRuns.mockResolvedValue(page([cronRun(3), cronRun(2), cronRun(1)]));
    renderFeed(parseFeedQuery(new URLSearchParams("sort=duration&order=desc")));
    expect(await screen.findByRole("link", { name: "api #3" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /×/ })).not.toBeInTheDocument();
  });

  it("labels an unknown actor No actor and a timer actor schedule in the Actor filter", async () => {
    mockListFleetRuns.mockResolvedValue(page([run({ actor: "unknown" }), cronRun(1)]));
    renderFeed();
    await screen.findByRole("link", { name: "api #1" });
    const select = screen.getByLabelText("Actor");
    expect(within(select).getByRole("option", { name: "No actor" })).toHaveAttribute("value", "unknown");
    expect(within(select).getByRole("option", { name: "schedule" })).toHaveAttribute("value", "timer");
  });
});
