import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { RUN_STATUSES, isTerminalRunStatus, runGlyph, runGlyphLabel } from "./runGlyph";

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
