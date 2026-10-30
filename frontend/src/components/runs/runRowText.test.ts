import { describe, it, expect } from "vitest";
import type { FleetRunRow } from "../../types";
import { runGlyph, runHref, runSecondLine, runTiming } from "./runRowText";

const NOW = Date.parse("2026-10-30T12:00:00Z");

function row(over: Partial<FleetRunRow> = {}): FleetRunRow {
  return {
    runId: "r1",
    status: "COMPLETED",
    result: "SUCCESS",
    cluster: "core",
    tenant: "pay",
    jobPath: "pay/api",
    buildNumber: 42,
    actor: "nate",
    branch: "main",
    title: "Fix the parser",
    trigger: { kind: "push", label: "Push" },
    pendingInput: false,
    canWrite: false,
    createdAt: new Date(NOW - 3600_000).toISOString(),
    ...over,
  };
}

describe("runGlyph", () => {
  it.each([
    ["pending input wins", row({ status: "RUNNING", pendingInput: true }), "waiting"],
    ["a queued run", row({ status: "QUEUED", result: undefined }), "queued"],
    ["a running run", row({ status: "RUNNING", result: undefined }), "running"],
    ["a success", row({ result: "SUCCESS" }), "success"],
    ["a failure", row({ result: "FAILURE" }), "failure"],
    ["a trigger failure reads as a failure", row({ result: "TRIGGER_FAILED" }), "failure"],
    ["an unstable run", row({ result: "UNSTABLE" }), "unstable"],
    ["an aborted run", row({ result: "ABORTED" }), "aborted"],
    ["an interrupted run", row({ result: "INTERRUPTED" }), "interrupted"],
    ["an unknown result", row({ result: undefined }), "other"],
  ])("%s", (_name, r, want) => {
    expect(runGlyph(r)).toBe(want);
  });
});

describe("runTiming", () => {
  it("names a queued run", () => {
    expect(runTiming(row({ status: "QUEUED", result: undefined }), NOW)).toBe("queued");
  });

  it("shows the elapsed time of a running run", () => {
    const r = row({ status: "RUNNING", result: undefined, startedAt: new Date(NOW - 184_000).toISOString() });
    expect(runTiming(r, NOW)).toBe("running · 3m 4s");
  });

  it("shows the wait of a run that waits for input", () => {
    const r = row({
      status: "RUNNING",
      result: undefined,
      pendingInput: true,
      startedAt: new Date(NOW - 120_000).toISOString(),
    });
    expect(runTiming(r, NOW)).toBe("waiting 2m 0s");
  });

  it("names the reason of an interrupted run", () => {
    expect(runTiming(row({ result: "INTERRUPTED", interruptedReason: "cell-lost" }), NOW)).toBe(
      "interrupted · cell-lost",
    );
  });

  it("shows the age and the duration of a completed run", () => {
    const r = row({ durationMs: 75_000, startedAt: new Date(NOW - 3600_000).toISOString() });
    expect(runTiming(r, NOW)).toBe("1h ago · 1m 15s");
  });
});

describe("runSecondLine", () => {
  it("names the tenant, the job, the build, the trigger, and the actor", () => {
    expect(runSecondLine(row())).toBe("pay / pay/api #42: Push by nate");
  });

  it("omits the actor when none is recorded", () => {
    expect(runSecondLine(row({ actor: undefined }))).toBe("pay / pay/api #42: Push");
  });
});

describe("runHref", () => {
  it("encodes the job path as one segment", () => {
    expect(runHref("pay", "pay/api", "r1")).toBe("/pipelines/pay/pay%2Fapi/r1");
  });
});
