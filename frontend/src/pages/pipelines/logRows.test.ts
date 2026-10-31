import { describe, expect, it } from "vitest";
import type { RunStage } from "../../types";
import type { BodyRow, GroupState, LogRow, StageInfo } from "./logRows";
import {
  EMPTY_GROUP_STATE,
  agentManifestFolds,
  buildLogRows,
  hiddenGroupOfLine,
  stageJumpIds,
  stageJumpIndex,
  stickyStage,
} from "./logRows";

/** A short label per row: L<n> line, H<id> header, C<id> continued header, G<seq> gap, F<key> fold. */
function rowLabel(r: LogRow): string {
  if (r.kind === "line") return `L${r.n}`;
  if (r.kind === "header") return `${r.continued ? "C" : "H"}${r.stageId}`;
  if (r.kind === "fold") return `F${r.key}${r.collapsed ? "-closed" : ""}`;
  return `G${r.seq}`;
}

const text = (n: number, body: string, stageId?: string): BodyRow => ({
  kind: "line",
  n,
  off: n,
  ts: 0,
  text: body,
  stageId,
});

const MANIFEST_HEADER = "Agent pod-1 is provisioned from template jnlp-agent";

function manifestBody(): BodyRow[] {
  return [
    text(1, "before", "12"),
    text(2, MANIFEST_HEADER, "12"),
    text(3, "---", "12"),
    text(4, "apiVersion: v1", "12"),
    text(5, "kind: Pod", "12"),
    text(6, ""),
    text(7, "[Pipeline] { (boom)", "14"),
  ];
}

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
    expect(rows.map(rowLabel)).toEqual([
      "H12",
      "L1",
      "L2",
      "H14",
      "L3",
      "H16",
      "L4",
      "C12",
      "L5",
      "C14",
      "L6",
      "H4",
      "L7",
      "C16",
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

describe("buildLogRows groups", () => {
  it("marks a later run of a stage as continued", () => {
    const rows = buildLogRows([text(1, "a", "12"), text(2, "b", "14"), text(3, "c", "12")], info);
    expect(rows.filter((r) => r.kind === "header")).toEqual([
      { kind: "header", stageId: "12", name: "Alpha", status: "SUCCESS", continued: false, collapsed: false, lineCount: 2 },
      { kind: "header", stageId: "14", name: "Bravo", status: "FAILURE", continued: false, collapsed: false, lineCount: 1 },
      { kind: "header", stageId: "12", name: "Alpha", status: "SUCCESS", continued: true, collapsed: false, lineCount: 2 },
    ]);
  });

  it("hides a collapsed stage's lines and keeps its headers with the line count", () => {
    const body: BodyRow[] = [text(1, "a", "12"), text(2, "b", "12"), text(3, "c", "14")];
    const state: GroupState = { collapsedStages: new Set(["12"]), expandedFolds: new Set() };
    const rows = buildLogRows(body, info, state);
    expect(rows.map(rowLabel)).toEqual(["H12", "H14", "L3"]);
    const header = rows[0];
    expect(header).toMatchObject({ kind: "header", collapsed: true, lineCount: 2 });
  });

  it("folds an agent pod manifest by default", () => {
    const rows = buildLogRows(manifestBody(), info);
    expect(rows.map(rowLabel)).toEqual(["H12", "L1", "L2", "F3-closed", "H14", "L7"]);
    expect(rows[3]).toEqual({ kind: "fold", key: 3, lineCount: 4, collapsed: true });
  });

  it("gives no fold row to a manifest inside a collapsed stage", () => {
    const state: GroupState = { collapsedStages: new Set(["12"]), expandedFolds: new Set() };
    const rows = buildLogRows(manifestBody(), info, state);
    expect(rows.map(rowLabel)).toEqual(["H12", "H14", "L7"]);
  });

  it("shows the manifest lines once its fold is expanded", () => {
    const state: GroupState = { collapsedStages: new Set(), expandedFolds: new Set([3]) };
    const rows = buildLogRows(manifestBody(), info, state);
    expect(rows.map(rowLabel)).toEqual(["H12", "L1", "L2", "F3", "L3", "L4", "L5", "L6", "H14", "L7"]);
  });

  it("ends a fold at its first empty line or before a [Pipeline] line", () => {
    expect(agentManifestFolds(manifestBody())).toEqual([{ start: 2, end: 6, key: 3 }]);
    const body: BodyRow[] = [
      text(1, "Agent pod-1 is provisioned from template jnlp-agent"),
      text(2, "---"),
      text(3, "apiVersion: v1"),
      text(4, "[Pipeline] step"),
      text(5, "after"),
    ];
    expect(agentManifestFolds(body)).toEqual([{ start: 1, end: 3, key: 2 }]);
  });

  it("gives no fold to a provisioning line with no dashes after it", () => {
    const body: BodyRow[] = [text(1, MANIFEST_HEADER), text(2, "plain"), text(3, "more")];
    expect(agentManifestFolds(body)).toEqual([]);
  });

  it("hiddenGroupOfLine names the collapsed stage and the fold", () => {
    const body = manifestBody();
    const state: GroupState = { collapsedStages: new Set(["12"]), expandedFolds: new Set() };
    expect(hiddenGroupOfLine(body, state, 4)).toEqual({ stageId: "12", foldKey: 3 });
    expect(hiddenGroupOfLine(body, state, 7)).toBeNull();
    expect(hiddenGroupOfLine(body, EMPTY_GROUP_STATE, 1)).toBeNull();
    expect(hiddenGroupOfLine(body, { collapsedStages: new Set(["14"]), expandedFolds: new Set() }, 7)).toEqual({
      stageId: "14",
    });
    expect(hiddenGroupOfLine(body, EMPTY_GROUP_STATE, 99)).toBeNull();
  });

  it("keeps one run across a line with no stage", () => {
    const rows = buildLogRows([line(1, "12"), line(2), line(3, "12")], info);
    expect(rows.map(rowLabel)).toEqual(["H12", "L1", "L2", "L3"]);
  });

  it("keeps one run across a line whose stage is not known", () => {
    const rows = buildLogRows([line(1, "12"), line(2, "99"), line(3, "12")], info);
    expect(rows.map(rowLabel)).toEqual(["H12", "L1", "L2", "L3"]);
  });

  it("marks a later run continued only after a line of another stage", () => {
    const body: BodyRow[] = [line(1, "12"), line(2), line(3, "14"), line(4), line(5, "12")];
    const rows = buildLogRows(body, info);
    expect(rows.map(rowLabel)).toEqual(["H12", "L1", "L2", "H14", "L3", "L4", "C12", "L5"]);
  });

  it("keeps a pipeline-level timeout line inside the stage run", () => {
    const resolve = new Map<string, StageInfo>([["6", { name: "Resolve", status: "FAILURE" }]]);
    const body: BodyRow[] = [
      text(8, "[Pipeline] stage"),
      text(9, "[Pipeline] { (Resolve)", "6"),
      text(10, "Timeout set to expire in 1 hr 0 min"),
      text(11, "[Pipeline] node", "6"),
    ];
    const rows = buildLogRows(body, resolve);
    expect(rows.map(rowLabel)).toEqual(["L8", "H6", "L9", "L10", "L11"]);
  });

  it("keeps one header for a collapsed stage across a line with no stage", () => {
    const body: BodyRow[] = [line(1, "12"), line(2), line(3, "12")];
    const state: GroupState = { collapsedStages: new Set(["12"]), expandedFolds: new Set() };
    const rows = buildLogRows(body, info, state);
    expect(rows.map(rowLabel)).toEqual(["H12", "L2"]);
    expect(rows[0]).toMatchObject({ kind: "header", collapsed: true, lineCount: 2 });
  });

  it("keeps one run across a hidden fold that ends on a line with no stage", () => {
    const body: BodyRow[] = [
      text(1, "before", "12"),
      text(2, MANIFEST_HEADER, "12"),
      text(3, "---", "12"),
      text(4, "apiVersion: v1", "12"),
      text(5, ""),
      text(6, "after", "12"),
    ];
    const rows = buildLogRows(body, info);
    expect(rows.map(rowLabel)).toEqual(["H12", "L1", "L2", "F3-closed", "L6"]);
  });
});
