import { describe, it, expect } from "vitest";
import type { FleetRunGroup, FleetRunRow } from "../../types";
import { createActivityEvent } from "../../test/factories";
import { fleetRun } from "../../test/overviewFactories";
import { buildDayFeed, groupDetail, groupTitle, tenantCounts, type OtherRow } from "./todayFeed";

/** run builds one finished, passed run of payments / release. */
function run(over: Partial<FleetRunRow> = {}): FleetRunRow {
  return fleetRun({ result: "SUCCESS", ...over });
}

/** group builds one job group. Its newest run defaults to a passed run. */
function group(over: Partial<FleetRunGroup> = {}): FleetRunGroup {
  const newest = over.newest ?? run();
  return {
    tenant: newest.tenant,
    tenantFolder: "",
    cluster: "core",
    jobPath: newest.jobPath ?? "release",
    runs: 1,
    passed: 1,
    failed: 0,
    firstAt: newest.createdAt,
    newest,
    ...over,
  };
}

describe("todayFeed", () => {
  it("folds two runs of one job into one row", () => {
    const g = group({
      runs: 2,
      passed: 2,
      failed: 0,
      firstAt: "2026-11-14T08:00:00Z",
      newest: run({ runId: "run-2", buildNumber: 102 }),
    });
    const feed = buildDayFeed([g], []);
    expect(feed.timed).toHaveLength(0);
    expect(feed.folds).toHaveLength(1);
    expect(feed.folds[0]).toMatchObject({ kind: "fold" });
    expect(groupTitle(g)).toBe("payments / release ran 2 times");
  });

  it("keeps a single human run as its own row", () => {
    const g = group({ newest: run({ runId: "run-40", buildNumber: 40, trigger: { kind: "manual", label: "manual" }, actor: "nathan" }) });
    const feed = buildDayFeed([g], []);
    expect(feed.folds).toHaveLength(0);
    expect(feed.timed).toHaveLength(1);
    expect(feed.timed[0]).toMatchObject({ kind: "run" });
    expect(groupTitle(g)).toBe("payments / release #40 passed");
    expect(groupDetail(g)).toBe("Started by nathan · 30s");
  });

  it("folds single machine passes into the other runs row with tenant counts", () => {
    const groups = [
      group({ newest: run({ runId: "a", buildNumber: 1 }), tenant: "payments" }),
      group({ newest: run({ runId: "b", buildNumber: 2 }), tenant: "payments" }),
      group({ newest: run({ runId: "c", buildNumber: 3, tenant: "member-b", jobPath: "smoke" }), tenant: "member-b", jobPath: "smoke" }),
    ];
    const feed = buildDayFeed(groups, []);
    expect(feed.timed).toHaveLength(0);
    expect(feed.folds).toHaveLength(1);
    const other = feed.folds[0] as OtherRow;
    expect(other).toMatchObject({ kind: "other", runs: 3 });
    expect(tenantCounts(other.groups)).toBe("payments (2), member-b (1)");
  });

  it("renders a lone other pass as its own row", () => {
    const feed = buildDayFeed([group({ newest: run({ runId: "solo", buildNumber: 9 }) })], []);
    expect(feed.folds).toHaveLength(0);
    expect(feed.timed).toHaveLength(1);
    expect(feed.timed[0]).toMatchObject({ kind: "run" });
  });

  it("orders timed rows newest first across runs and events", () => {
    const human = group({
      newest: run({
        runId: "human",
        trigger: { kind: "manual", label: "manual" },
        endedAt: "2026-11-14T08:40:00Z",
      }),
    });
    const later = group({
      newest: run({ runId: "later", trigger: { kind: "manual", label: "manual" }, endedAt: "2026-11-14T09:10:00Z" }),
    });
    const events = [createActivityEvent({ timestamp: "2026-11-14T07:55:00Z", message: "member-b: 3 plugins updated" })];
    const feed = buildDayFeed([human, later], events);
    const order = feed.timed.map((r) => (r.kind === "run" ? r.group.newest.runId : r.event.message));
    expect(order).toEqual(["later", "human", "member-b: 3 plugins updated"]);
  });

  it("folds consecutive repeats of one event", () => {
    const same = { message: "Controller test-ctrl restarted", controller: "test-ctrl", namespace: "default", cluster: "core" };
    const events = [
      createActivityEvent({ ...same, timestamp: "2026-11-14T10:03:00Z" }),
      createActivityEvent({ ...same, timestamp: "2026-11-14T10:02:00Z" }),
      createActivityEvent({ ...same, timestamp: "2026-11-14T10:01:00Z" }),
      createActivityEvent({ message: "ledger-ctrl went to sleep", controller: "ledger-ctrl", namespace: "default", timestamp: "2026-11-14T09:00:00Z" }),
    ];
    const feed = buildDayFeed([], events);
    expect(feed.timed).toHaveLength(2);
    expect(feed.timed[0]).toMatchObject({ kind: "event", repeats: 3 });
    expect(feed.timed[1]).toMatchObject({ kind: "event", repeats: 1 });
  });

  it("names the failed count in a fold detail", () => {
    const g = group({
      runs: 3,
      passed: 2,
      failed: 1,
      firstAt: "2026-11-14T08:00:00Z",
      newest: run({ runId: "run-3", buildNumber: 103, result: "SUCCESS", endedAt: "2026-11-14T11:02:00Z" }),
    });
    expect(groupDetail(g)).toMatch(/^1 failed · 2 passed · \d\d:\d\d to \d\d:\d\d$/);
  });
});
