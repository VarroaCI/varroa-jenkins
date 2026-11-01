import { describe, it, expect } from "vitest";
import type { FleetRunRow } from "../../types";
import { DEFAULT_FEED_QUERY, parseFeedQuery } from "./feedQuery";
import { hasStateFilter, isDefaultSort, mergeFirstPage } from "./feedLive";

function run(id: string, status = "COMPLETED"): FleetRunRow {
  return {
    runId: id,
    status,
    cluster: "core",
    tenant: "pay",
    tenantFolder: "pay",
    trigger: { kind: "push", label: "Push" },
    pendingInput: false,
    canWrite: false,
    createdAt: "2026-10-30T10:00:00Z",
  };
}

describe("mergeFirstPage", () => {
  it("prepends a new run", () => {
    const merged = mergeFirstPage([[run("r2"), run("r1")]], [run("r3")]);
    expect(merged.map((p) => p.map((r) => r.runId))).toEqual([["r3", "r2", "r1"]]);
  });

  it("updates a known row in place on a later page", () => {
    const merged = mergeFirstPage(
      [[run("r2")], [run("r1", "RUNNING")]],
      [run("r1", "COMPLETED")],
    );
    expect(merged.map((p) => p.map((r) => r.runId))).toEqual([["r2"], ["r1"]]);
    expect(merged[1][0].status).toBe("COMPLETED");
  });

  it("keeps older pages", () => {
    const merged = mergeFirstPage(
      [[run("r2")], [run("r1")]],
      [run("r3"), run("r2", "RUNNING")],
    );
    expect(merged.map((p) => p.map((r) => r.runId))).toEqual([["r3", "r2"], ["r1"]]);
    expect(merged[0][1].status).toBe("RUNNING");
  });
});

describe("hasStateFilter", () => {
  it("is false for a sort or a tenant alone", () => {
    expect(hasStateFilter(parseFeedQuery(new URLSearchParams("sort=duration&order=asc")))).toBe(false);
    expect(hasStateFilter(parseFeedQuery(new URLSearchParams("tenant=pay")))).toBe(false);
  });

  it("is true for a result or a status", () => {
    expect(hasStateFilter(parseFeedQuery(new URLSearchParams("result=FAILURE")))).toBe(true);
    expect(hasStateFilter(parseFeedQuery(new URLSearchParams("status=RUNNING")))).toBe(true);
  });
});

describe("isDefaultSort", () => {
  it("is true for the started sort in descending order", () => {
    expect(isDefaultSort(DEFAULT_FEED_QUERY)).toBe(true);
  });

  it("is false for any other sort or order", () => {
    expect(isDefaultSort(parseFeedQuery(new URLSearchParams("sort=duration")))).toBe(false);
    expect(isDefaultSort(parseFeedQuery(new URLSearchParams("order=asc")))).toBe(false);
  });
});
