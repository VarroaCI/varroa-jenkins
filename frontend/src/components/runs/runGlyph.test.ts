import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  RUN_STATUSES,
  isTerminalRunStatus,
  runGlyph,
  runGlyphLabel,
  runLabel,
  runResultLabel,
  runTone,
} from "./runGlyph";

// The record store defines the run statuses the API answers. This file
// lives at frontend/src/components/runs/, so four levels up is the repo root.
const statusSource = join(import.meta.dirname, "../../../../internal/recordstore/ingest_common.go");

/** goRunStatuses reads the values of the runs.status const block of the record store. */
function goRunStatuses(): string[] {
  const src = readFileSync(statusSource, "utf8");
  const block = /\/\/ runs\.status values\.[\s\S]*?const \(([\s\S]*?)\)/.exec(src);
  if (!block) throw new Error("ingest_common.go holds no runs.status const block");
  return [...block[1].matchAll(/=\s*"([A-Z_]+)"/g)].map((m) => m[1]);
}

describe("RUN_STATUSES", () => {
  it("lists every runs.status value of the record store, and no other", () => {
    const go = goRunStatuses();
    expect(go.length).toBeGreaterThan(0);
    expect([...RUN_STATUSES].sort()).toEqual([...go].sort());
  });

  it.each(goRunStatuses().filter((s) => s !== "COMPLETED"))("gives %s a status mark of its own", (status) => {
    const glyph = runGlyph({ status });
    expect(glyph).not.toBe("other");
    if (status !== "QUEUED") expect(runGlyphLabel(glyph)).not.toBe("Queued");
  });

  it("gives a COMPLETED run the mark of its result", () => {
    expect(runGlyph({ status: "COMPLETED", result: "SUCCESS" })).toBe("success");
    expect(runGlyph({ status: "COMPLETED", result: "ABORTED" })).toBe("aborted");
  });
});

describe("runGlyph for a DUPLICATE run", () => {
  it.each([
    ["with no result", undefined],
    ["with a stored ABORTED result", "ABORTED"],
    ["with a stored SUCCESS result", "SUCCESS"],
  ])("answers duplicate %s", (_name, result) => {
    expect(runGlyph({ status: "DUPLICATE", result })).toBe("duplicate");
    expect(runGlyphLabel("duplicate")).toBe("Duplicate");
  });
});

// The runs.result values that the record store accepts. The CHECK
// constraint runs_result_check holds the same list.
describe("runResultLabel", () => {
  it.each([
    ["SUCCESS", "Passed", "ok"],
    ["FAILURE", "Failed", "bad"],
    ["TRIGGER_FAILED", "Failed", "bad"],
    ["UNSTABLE", "Unstable", "warn"],
    ["ABORTED", "Aborted", "idle"],
    ["NOT_BUILT", "Not built", "idle"],
    ["INTERRUPTED", "Interrupted", "bad"],
  ])("names the %s result %s with the %s tone", (result, label, tone) => {
    expect(runResultLabel(result)).toBe(label);
    expect(runTone(runGlyph({ status: "COMPLETED", result }))).toBe(tone);
  });

  it("keeps an unknown result as its raw value", () => {
    expect(runResultLabel("SOMETHING_NEW")).toBe("SOMETHING_NEW");
  });
});

describe("runLabel", () => {
  it.each([
    [{ status: "QUEUED" }, "Queued", "info"],
    [{ status: "RUNNING" }, "Running", "info"],
    [{ status: "RUNNING", pendingInput: true }, "Waiting for input", "warn"],
    [{ status: "DUPLICATE", result: "SUCCESS" }, "Duplicate", "idle"],
    [{ status: "COMPLETED", result: "SUCCESS" }, "Passed", "ok"],
  ])("names %o %s with the %s tone", (run, label, tone) => {
    expect(runLabel(run)).toBe(label);
    expect(runTone(runGlyph(run))).toBe(tone);
  });

  it("keeps the raw status of a state with no mark", () => {
    expect(runLabel({ status: "COMPLETED" })).toBe("COMPLETED");
    expect(runLabel({ status: "SOMETHING_NEW" })).toBe("SOMETHING_NEW");
  });
});

describe("isTerminalRunStatus", () => {
  it.each([
    ["QUEUED", false],
    ["RUNNING", false],
    ["COMPLETED", true],
    ["DUPLICATE", true],
    ["WAITING_INPUT", false],
    ["", false],
  ])("answers %s as terminal %s", (status, want) => {
    expect(isTerminalRunStatus(status)).toBe(want);
  });
});
