import { describe, expect, it } from "vitest";
import type { RunStage } from "../../types";
import type { BodyRow, LogRow, StageInfo } from "./logRows";
import { buildLogRows, stageJumpIds, stageJumpIndex, stickyStage } from "./logRows";

const line = (n: number, stageId?: string): BodyRow => ({
  kind: "line",
  n,
  off: n,
  ts: 0,
  text: `L${n}`,
  stageId,
});

const stage = (stageId: string, parentId?: string): RunStage => ({
  stageId,
  name: `S${stageId}`,
  status: "SUCCESS",
  parentId,
});

const info = new Map<string, StageInfo>([
  ["4", { name: "Fan", status: "FAILURE" }],
  ["8", { name: "Branch: Alpha", status: "SUCCESS" }],
  ["12", { name: "Alpha", status: "SUCCESS" }],
  ["14", { name: "Bravo", status: "FAILURE" }],
  ["16", { name: "Charlie", status: "SUCCESS" }],
]);

function headerIds(rows: readonly LogRow[]): string[] {
  return rows.flatMap((r, i) => (r.kind === "header" ? [`${i}:${r.stageId}`] : []));
}

describe("buildLogRows", () => {
  it("places each header before its stage's first line", () => {
    const body: BodyRow[] = [
      line(1, "12"),
      line(2, "12"),
      line(3, "14"),
      line(4, "16"),
      line(5, "12"),
      line(6, "14"),
      line(7, "4"),
      line(8, "16"),
    ];
    const rows = buildLogRows(body, info);
    expect(rows.map((r) => (r.kind === "line" ? `L${r.n}` : r.kind === "header" ? `H${r.stageId}` : r.kind))).toEqual([
      "H12",
      "L1",
      "L2",
      "H14",
      "L3",
      "H16",
      "L4",
      "L5",
      "L6",
      "H4",
      "L7",
      "L8",
    ]);
  });

  it("places no header for a stage with no line, or for a line with an empty stageId", () => {
    const rows = buildLogRows([line(1, ""), line(2)], info);
    expect(rows.some((r) => r.kind === "header")).toBe(false);
    expect(rows).toHaveLength(2);
  });

  it("places no header for a line whose stage is not yet known, and places it before that first line once the stage is known", () => {
    const body: BodyRow[] = [line(1, "12"), line(2, "12")];
    expect(buildLogRows(body, new Map())).toHaveLength(2);
    expect(headerIds(buildLogRows(body, info))).toEqual(["0:12"]);
  });
});

describe("stageJumpIds", () => {
  it("walks parentId to every descendant", () => {
    const stages = [
      stage("4"),
      stage("8", "4"),
      stage("12", "8"),
      stage("14", "8"),
      stage("16", "4"),
      stage("20"),
    ];
    expect([...stageJumpIds("4", stages)].sort()).toEqual(["12", "14", "16", "4", "8"]);
    expect([...stageJumpIds("20", stages)]).toEqual(["20"]);
    expect([...stageJumpIds("99", [])]).toEqual(["99"]);
  });
});

describe("stageJumpIndex", () => {
  it("picks the earliest descendant header", () => {
    const body: BodyRow[] = [line(1, "12"), line(2, "4"), line(3, "16")];
    const rows = buildLogRows(body, info);
    const stages = [stage("4"), stage("8", "4"), stage("12", "8"), stage("16", "4")];
    // Fan (4) has its own late header at index 2; the earliest of its family is Alpha (12) at 0.
    expect(stageJumpIndex(rows, stageJumpIds("4", stages))).toBe(0);
    expect(stageJumpIndex(rows, stageJumpIds("14", stages))).toBe(-1);
  });
});

describe("stickyStage", () => {
  it("names the first visible line stage and skips lines of unknown stages", () => {
    const rows = buildLogRows([line(1, "12"), line(2, "12"), line(3, "4")], info);
    // rows: 0 H12, 1 L1, 2 L2, 3 H4, 4 L3
    expect(stickyStage(rows, 4, info)).toEqual({ name: "Fan", status: "FAILURE" });
    expect(stickyStage(rows, 2, info)).toEqual({ name: "Alpha", status: "SUCCESS" });
    // A line whose stage is unknown is skipped; the nearest known line names the view.
    const fallback = buildLogRows([line(1, "12"), line(2, "99")], info);
    expect(stickyStage(fallback, 2, info)).toEqual({ name: "Alpha", status: "SUCCESS" });
    // A header at the first row names its own stage, not the stage above it.
    const boundary = buildLogRows([line(1, "12"), line(2, "14")], info);
    // rows: 0 H12, 1 L1, 2 H14, 3 L2
    expect(stickyStage(boundary, 2, info)).toEqual({ name: "Bravo", status: "FAILURE" });
    expect(stickyStage([], 0, info)).toBeNull();
  });
});
