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

/**
 * A stage header. The viewer derives it from the lines' stage attribution.
 * A continued header marks a later run of a stage that already has one.
 * A collapsed header hides the lines of its stage.
 */
export type HeaderRow = {
  kind: "header";
  stageId: string;
  name: string;
  status: string;
  continued: boolean;
  collapsed: boolean;
  lineCount: number;
};

/**
 * The row that stands for one folded agent pod manifest. The key is the n
 * of the manifest's dashes line, which names the fold across renders.
 */
export type FoldRow = { kind: "fold"; key: number; lineCount: number; collapsed: boolean };

export type LogRow = BodyRow | HeaderRow | FoldRow;

/** The viewer state that decides which rows are hidden. */
export interface GroupState {
  collapsedStages: ReadonlySet<string>;
  expandedFolds: ReadonlySet<number>;
}

/** The state of a viewer that has not collapsed or expanded anything. */
export const EMPTY_GROUP_STATE: GroupState = {
  collapsedStages: new Set<string>(),
  expandedFolds: new Set<number>(),
};

/** The display name and status of a stage, as the latest stage frame reports them. */
export interface StageInfo {
  name: string;
  status: string;
}

const AGENT_PROVISIONED = /^Agent \S+ is provisioned from template \S+/;

/** One agent pod manifest fold: body indexes with end exclusive, and the dashes line's n. */
export interface AgentManifestFold {
  start: number;
  end: number;
  key: number;
}

function trimCR(text: string): string {
  return text.endsWith("\r") ? text.slice(0, -1) : text;
}

/**
 * Finds each agent provisioning block, with the same rule as the server's
 * dropAgentPodManifests. A block is a header line, then a dashes line, then
 * the pod manifest through its first empty line. A [Pipeline] line or a gap
 * ends the block early. The fold starts at the dashes line, so the header
 * stays visible. A header with no dashes line after it gives no fold.
 */
export function agentManifestFolds(body: readonly BodyRow[]): AgentManifestFold[] {
  const folds: AgentManifestFold[] = [];
  let i = 0;
  while (i < body.length) {
    const row = body[i];
    const next = body[i + 1];
    if (
      row.kind !== "line" ||
      !AGENT_PROVISIONED.test(trimCR(row.text)) ||
      next === undefined ||
      next.kind !== "line" ||
      trimCR(next.text) !== "---"
    ) {
      i++;
      continue;
    }
    let j = i + 2;
    while (j < body.length) {
      const cur = body[j];
      if (cur.kind !== "line") break;
      const t = trimCR(cur.text);
      if (t.startsWith("[Pipeline] ")) break;
      j++;
      if (t === "") break;
    }
    folds.push({ start: i + 1, end: j, key: next.n });
    i = j;
  }
  return folds;
}

/**
 * Places a header row before each run of one stage's lines. Only a line of
 * another known stage ends a run. A line with no known stage stays in the
 * run around it. A later run of a stage that already has a header gets a
 * continued header. A collapsed stage keeps its headers and hides its
 * lines. An agent manifest that is not expanded hides its lines behind one
 * fold row.
 */
export function buildLogRows(
  body: readonly BodyRow[],
  stageInfo: ReadonlyMap<string, StageInfo>,
  state: GroupState = EMPTY_GROUP_STATE,
): LogRow[] {
  // stageOf answers the known stage of a body row, or "" when it has none.
  const stageOf = (row: BodyRow): string =>
    row.kind === "line" && row.stageId && stageInfo.has(row.stageId) ? row.stageId : "";

  const lineCounts = new Map<string, number>();
  for (const row of body) {
    const sid = stageOf(row);
    if (sid !== "") lineCounts.set(sid, (lineCounts.get(sid) ?? 0) + 1);
  }
  const foldAt = new Map<number, AgentManifestFold>();
  for (const f of agentManifestFolds(body)) foldAt.set(f.start, f);

  const out: LogRow[] = [];
  const placed = new Set<string>();
  let prevStage = "";

  let i = 0;
  while (i < body.length) {
    const fold = foldAt.get(i);
    if (fold) {
      const expanded = state.expandedFolds.has(fold.key);
      // A fold inside a collapsed stage is hidden with that stage, so it gets no row.
      // The provisioning header and the dashes line may carry different stages; either one hides the fold.
      const headerStage = fold.start > 0 ? stageOf(body[fold.start - 1]) : "";
      const inCollapsed =
        state.collapsedStages.has(stageOf(body[fold.start])) || state.collapsedStages.has(headerStage);
      if (!inCollapsed) {
        out.push({ kind: "fold", key: fold.key, lineCount: fold.end - fold.start, collapsed: !expanded });
      }
      if (inCollapsed || !expanded) {
        for (let k = fold.start; k < fold.end; k++) {
          const s = stageOf(body[k]);
          if (s !== "") prevStage = s;
        }
        i = fold.end;
        continue;
      }
    }

    const row = body[i];
    i++;
    if (row.kind !== "line") {
      out.push(row);
      continue;
    }
    const sid = stageOf(row);
    if (sid === "") {
      // A line with no known stage keeps prevStage, so it never splits a run.
      out.push(row);
      continue;
    }
    if (sid !== prevStage) {
      const info = stageInfo.get(sid)!;
      const continued = placed.has(sid);
      placed.add(sid);
      out.push({
        kind: "header",
        stageId: sid,
        name: info.name,
        status: info.status,
        continued,
        collapsed: state.collapsedStages.has(sid),
        lineCount: lineCounts.get(sid) ?? 0,
      });
    }
    prevStage = sid;
    if (!state.collapsedStages.has(sid)) out.push(row);
  }
  return out;
}

/**
 * Names what hides a line: its collapsed stage and its unexpanded agent
 * manifest fold. It returns null for a line that no group hides, and for a
 * line number the body does not hold.
 */
export function hiddenGroupOfLine(
  body: readonly BodyRow[],
  state: GroupState,
  n: number,
): { stageId?: string; foldKey?: number } | null {
  const idx = body.findIndex((r) => r.kind === "line" && r.n === n);
  if (idx === -1) return null;
  const row = body[idx] as LineRow;
  const out: { stageId?: string; foldKey?: number } = {};
  if (row.stageId && state.collapsedStages.has(row.stageId)) out.stageId = row.stageId;
  const fold = agentManifestFolds(body).find((f) => idx >= f.start && idx < f.end);
  if (fold && !state.expandedFolds.has(fold.key)) out.foldKey = fold.key;
  return out.stageId !== undefined || out.foldKey !== undefined ? out : null;
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
