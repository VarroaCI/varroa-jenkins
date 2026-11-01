import { describe, it, expect } from "vitest";
import type { FleetRunRow } from "../../types";
import { FOLD_MAX, foldRuns, foldSecondLine, foldTitle, type FeedItem } from "./runFold";

const NOW = Date.parse("2026-10-30T12:00:00Z");

/** row builds one finished cron run of smoke-a/cron-probe, numbered n. */
function row(n: number, over: Partial<FleetRunRow> = {}): FleetRunRow {
  return {
    runId: `r${n}`,
    status: "COMPLETED",
    result: "SUCCESS",
    cluster: "core",
    tenant: "smoke-a",
    tenantFolder: "smoke-a",
    jobPath: "smoke-a/cron-probe",
    buildNumber: n,
    actor: "timer",
    trigger: { kind: "cron", label: "Cron" },
    pendingInput: false,
    canWrite: false,
    createdAt: new Date(NOW - n * 60_000).toISOString(),
    ...over,
  };
}

/** shape names each item of a feed: a run, or a fold with its run count. */
function shape(items: FeedItem[]): string[] {
  return items.map((item) => (item.kind === "run" ? "run" : `fold:${item.runs.length}`));
}

describe("foldRuns", () => {
  it("folds consecutive runs of one job into one fold", () => {
    expect(shape(foldRuns([row(3), row(2), row(1)]))).toEqual(["fold:3"]);
  });

  it("keeps one run as a row", () => {
    expect(shape(foldRuns([row(1)]))).toEqual(["run"]);
  });

  it("breaks the fold at a run with a different result", () => {
    const items = foldRuns([row(3), row(2), row(1, { result: "FAILURE" })]);
    expect(shape(items)).toEqual(["fold:2", "run"]);
  });

  it("never folds a run with a commit subject", () => {
    const items = foldRuns([
      row(3, { changeSummary: "Fix the parser" }),
      row(2, { changeSummary: "Fix the parser" }),
      row(1, { changeSummary: "Fix the parser" }),
    ]);
    expect(shape(items)).toEqual(["run", "run", "run"]);
  });

  it("keeps a running run and a run that waits for input out of a fold, and breaks the fold there", () => {
    const running = row(2, { status: "RUNNING", result: undefined });
    const waiting = row(2, { status: "RUNNING", result: undefined, pendingInput: true });
    expect(shape(foldRuns([row(3), running, row(1)]))).toEqual(["run", "run", "run"]);
    expect(shape(foldRuns([row(3), waiting, row(1)]))).toEqual(["run", "run", "run"]);
  });

  it("caps a fold at FOLD_MAX runs and starts a new fold after it", () => {
    const rows = Array.from({ length: FOLD_MAX + 1 }, (_, i) => row(FOLD_MAX + 1 - i));
    expect(shape(foldRuns(rows))).toEqual([`fold:${FOLD_MAX}`, "run"]);
  });

  it("breaks the fold at a run on another branch", () => {
    const items = foldRuns([row(3, { branch: "main" }), row(2, { branch: "release/2" }), row(1, { branch: "release/2" })]);
    expect(shape(items)).toEqual(["run", "fold:2"]);
  });

  it("keeps the fold id when older runs load at the tail", () => {
    const before = foldRuns([row(3), row(2)]);
    const after = foldRuns([row(3), row(2), row(1)]);
    if (before[0].kind !== "fold" || after[0].kind !== "fold") throw new Error("expected folds");
    expect(after[0].id).toBe(before[0].id);
  });

  it("keeps the fold id when a newer run joins the top", () => {
    const before = foldRuns([row(3), row(2), row(1)]);
    const after = foldRuns([row(4), row(3), row(2), row(1)]);
    expect(before[0].kind === "fold" && after[0].kind === "fold").toBe(true);
    if (before[0].kind !== "fold" || after[0].kind !== "fold") return;
    expect(after[0].id).toBe(before[0].id);
  });
});

describe("foldTitle and foldSecondLine", () => {
  it("names the job and the run count", () => {
    expect(foldTitle([row(3), row(2), row(1)])).toBe("cron-probe × 3");
  });

  it("names the tenant, the outcome, and the build range", () => {
    expect(foldSecondLine([row(3), row(2), row(1)])).toBe("smoke-a · all passed · #1–#3");
  });

  it("leaves out the build range when a run has no build number", () => {
    expect(foldSecondLine([row(3), row(2, { buildNumber: undefined }), row(1)])).toBe("smoke-a · all passed");
  });

  it.each([
    ["FAILURE", "all failed"],
    ["UNSTABLE", "all unstable"],
    ["ABORTED", "all aborted"],
    ["INTERRUPTED", "all interrupted"],
    ["NOT_BUILT", "none built"],
  ])("names a %s fold", (result, text) => {
    expect(foldSecondLine([row(2, { result }), row(1, { result })])).toBe(`smoke-a · ${text} · #1–#2`);
  });
});
