import type { RunStage } from "../../types";

/** A log row that carries one console line. */
export type LineRow = {
  kind: "line";
  n: number;
  off: number;
  ts: number;
  text: string;
  stageId?: string;
  nodeId?: string;
};

/** A row that marks a gap in the received bytes. */
export type GapRow = { kind: "gap"; seq: number; off: number };

/** A row that the server sends as a line or a gap. */
export type BodyRow = LineRow | GapRow;

/** A stage header. The viewer derives it from the lines' stage attribution. */
export type HeaderRow = { kind: "header"; stageId: string; name: string; status: string };

export type LogRow = BodyRow | HeaderRow;

/** The display name and status of a stage, as the latest stage frame reports them. */
export interface StageInfo {
  name: string;
  status: string;
}

/**
 * Places a header row before the first line of each stage. A stage that
 * has no line of its own gets no header. A line whose stage is not yet
 * known gets no header until the stage frame arrives.
 */
export function buildLogRows(
  body: readonly BodyRow[],
  stageInfo: ReadonlyMap<string, StageInfo>,
): LogRow[] {
  const out: LogRow[] = [];
  const placed = new Set<string>();
  for (const row of body) {
    if (row.kind === "line" && row.stageId) {
      const info = stageInfo.get(row.stageId);
      if (info && !placed.has(row.stageId)) {
        placed.add(row.stageId);
        out.push({ kind: "header", stageId: row.stageId, name: info.name, status: info.status });
      }
    }
    out.push(row);
  }
  return out;
}

/** The stage ids a selection of stageId resolves to: the stage and all its descendants. */
export function stageJumpIds(stageId: string, stages: readonly RunStage[]): Set<string> {
  const children = new Map<string, string[]>();
  for (const s of stages) {
    if (!s.parentId) continue;
    const list = children.get(s.parentId) ?? [];
    list.push(s.stageId);
    children.set(s.parentId, list);
  }
  const ids = new Set<string>([stageId]);
  const queue = [stageId];
  for (let i = 0; i < queue.length; i++) {
    for (const child of children.get(queue[i]) ?? []) {
      if (ids.has(child)) continue;
      ids.add(child);
      queue.push(child);
    }
  }
  return ids;
}

/** The index of the earliest header whose stage is one of ids, or -1. */
export function stageJumpIndex(rows: readonly LogRow[], ids: ReadonlySet<string>): number {
  return rows.findIndex((r) => r.kind === "header" && ids.has(r.stageId));
}

/**
 * The stage that names the view's first row. Walking up from firstIndex, it
 * is the nearest header, or the stage of the nearest line whose stage is
 * known. A header at firstIndex therefore wins over the stage above it.
 */
export function stickyStage(
  rows: readonly LogRow[],
  firstIndex: number,
  stageInfo: ReadonlyMap<string, StageInfo>,
): StageInfo | null {
  for (let i = Math.min(firstIndex, rows.length - 1); i >= 0; i--) {
    const r = rows[i];
    if (r.kind === "header") return { name: r.name, status: r.status };
    if (r.kind === "line" && r.stageId) {
      const info = stageInfo.get(r.stageId);
      if (info) return info;
    }
  }
  return null;
}
