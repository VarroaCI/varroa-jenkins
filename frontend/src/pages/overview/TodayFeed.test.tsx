import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TodayFeed } from "./TodayFeed";
import { renderWithProviders } from "../../test/render-utils";
import { createActivityEvent } from "../../test/factories";
import { fleetRun } from "../../test/overviewFactories";
import { ApiError } from "../../hooks/useApi";
import type { ActivityPage, FleetRunGroup, FleetRunGroupPage } from "../../types";

const mocks = vi.hoisted(() => ({ listFleetRunGroups: vi.fn(), listActivity: vi.fn() }));
vi.mock("../../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/client")>()),
  ...mocks,
}));

/** group builds one job group of payments / release with a passed newest run. */
function group(over: Partial<FleetRunGroup> = {}): FleetRunGroup {
  const newest = fleetRun({ result: "SUCCESS", runId: "run-9", buildNumber: 9, trigger: { kind: "push", label: "push" } });
  return {
    tenant: "payments",
    tenantFolder: "",
    cluster: "core",
    jobPath: "release",
    runs: 2,
    passed: 2,
    failed: 0,
    firstAt: "2026-11-14T08:00:00Z",
    newest,
    ...over,
  };
}

function groupPage(over: Partial<FleetRunGroupPage> = {}): FleetRunGroupPage {
  return { since: "2026-11-14T00:00:00Z", groups: [], held: 0, more: false, ...over };
}

function activityPage(over: Partial<ActivityPage> = {}): ActivityPage {
  return { items: [], hasMore: false, retentionMode: "on", ...over };
}

function renderFeed(canPipelines = true) {
  return renderWithProviders(<TodayFeed canPipelines={canPipelines} />);
}

describe("TodayFeed", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.listFleetRunGroups.mockResolvedValue(groupPage());
    mocks.listActivity.mockResolvedValue(activityPage());
  });

  it("reads the groups and the activity since local midnight", async () => {
    renderFeed();
    await screen.findByText("Nothing ran and nothing changed since midnight.");
    const { since } = mocks.listFleetRunGroups.mock.calls[0][0] as { since: string };
    const local = new Date(since);
    expect(local.getHours()).toBe(0);
    expect(local.getMinutes()).toBe(0);
    expect(mocks.listActivity).toHaveBeenCalledWith(expect.objectContaining({ start: since }));
  });

  it("asks the activity read to leave out build events, routine sign-ins, and routine events", async () => {
    renderFeed();
    await screen.findByText("Nothing ran and nothing changed since midnight.");
    expect(mocks.listActivity).toHaveBeenCalledWith(
      expect.objectContaining({ excludeBuildEvents: true, excludeRoutineSignIns: true, excludeRoutineEvents: true }),
    );
  });

  it("shows eight timed rows, then the job fold", async () => {
    const events = Array.from({ length: 12 }, (_, i) =>
      createActivityEvent({
        message: `ledger-ctrl event ${i}`,
        controller: "ledger-ctrl",
        namespace: "default",
        timestamp: `2026-11-14T10:${String(59 - i).padStart(2, "0")}:00Z`,
      }),
    );
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups: [group()] }));
    mocks.listActivity.mockResolvedValue(activityPage({ items: events }));
    renderFeed();
    const fold = await screen.findByText("payments / release ran 2 times");
    expect(screen.getByText("ledger-ctrl event 7")).toBeInTheDocument();
    expect(screen.queryByText("ledger-ctrl event 8")).not.toBeInTheDocument();
    const lastTimed = screen.getByText("ledger-ctrl event 7");
    expect(lastTimed.compareDocumentPosition(fold) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show all 12" })).toBeInTheDocument();
  });

  it("caps the job folds at five", async () => {
    const groups = Array.from({ length: 7 }, (_, i) => group({ jobPath: `job-${i}` }));
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups }));
    renderFeed();
    await screen.findByText("payments / job-0 ran 2 times");
    expect(screen.getAllByText(/ran 2 times$/)).toHaveLength(5);
    await userEvent.click(screen.getByRole("button", { name: "Show all 7 jobs" }));
    expect(screen.getAllByText(/ran 2 times$/)).toHaveLength(7);
  });

  it("shows the held-failure note", async () => {
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups: [group()], held: 1 }));
    renderFeed();
    expect(await screen.findByText("1 failed run is in the list above.")).toBeInTheDocument();
  });

  it("Show links the command center view of the job", async () => {
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups: [group()] }));
    renderFeed();
    const show = await screen.findByRole("link", { name: "Show" });
    const { since } = mocks.listFleetRunGroups.mock.calls[0][0] as { since: string };
    const url = new URL(show.getAttribute("href") ?? "", "http://localhost");
    expect(url.pathname).toBe("/pipelines");
    expect(url.searchParams.get("tenant")).toBe("payments");
    expect(url.searchParams.get("job")).toBe("release");
    expect(url.searchParams.get("since")).toBe(since);
  });

  it("Show expands the other runs row", async () => {
    const other = (n: number, tenant: string) =>
      group({
        tenant,
        runs: 1,
        passed: 1,
        newest: fleetRun({ result: "SUCCESS", runId: `o-${n}`, buildNumber: n, tenant, jobPath: "smoke", trigger: { kind: "cron", label: "cron" } }),
        jobPath: "smoke",
      });
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups: [other(5, "payments"), other(6, "member-b")] }));
    renderFeed();
    expect(await screen.findByText("2 other runs passed")).toBeInTheDocument();
    expect(screen.getByText("member-b (1), payments (1)")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show" }));
    expect(screen.getByRole("link", { name: "payments / smoke #5" })).toHaveAttribute(
      "href",
      "/pipelines/payments/smoke/o-5",
    );
  });

  it("keeps the runs when the activity read fails", async () => {
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups: [group()] }));
    mocks.listActivity.mockRejectedValue(new Error("boom"));
    renderFeed();
    expect(await screen.findByText("Events are not listed: Varroa could not load them.")).toBeInTheDocument();
    expect(screen.getByText("payments / release ran 2 times")).toBeInTheDocument();
  });

  it("keeps the events when pipeline data is off", async () => {
    mocks.listFleetRunGroups.mockRejectedValue(new ApiError(503, "record store disabled"));
    mocks.listActivity.mockResolvedValue(activityPage({ items: [createActivityEvent({ message: "ledger-ctrl went to sleep" })] }));
    renderFeed();
    expect(await screen.findByText("Runs are not listed: pipeline data is off.")).toBeInTheDocument();
    expect(screen.getByText("ledger-ctrl went to sleep")).toBeInTheDocument();
  });

  it("says nothing ran today", async () => {
    renderFeed();
    expect(await screen.findByText("Nothing ran and nothing changed since midnight.")).toBeInTheDocument();
  });

  it("names more runs on the Pipelines page", async () => {
    mocks.listFleetRunGroups.mockResolvedValue(groupPage({ groups: [group()], more: true }));
    renderFeed();
    const link = await screen.findByRole("link", { name: "More runs are on the Pipelines page." });
    expect(link).toHaveAttribute("href", "/pipelines");
  });

  it("says the runs are not read without pipelines access", async () => {
    renderFeed(false);
    await screen.findByText("Runs are not listed: you cannot read pipelines.");
    expect(mocks.listFleetRunGroups).not.toHaveBeenCalled();
  });
});
