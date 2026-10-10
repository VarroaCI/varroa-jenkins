import { useEffect, useMemo, useState } from "react";
import type { RunLogStageEvent, RunStage } from "../../types";
import styles from "./StageGraph.module.css";

/** The subset of useEventStream's frame shape StageGraph reads. */
export interface StageGraphEvent {
  type: string;
  data: unknown;
}

export interface StageGraphProps {
  /** The stage list from GetRun, read once on load. */
  stages: RunStage[];
  /** The build page's shared log-stream frame; StageGraph reads only "stage" frames. */
  lastEvent?: StageGraphEvent | null;
  /** The run's terminal-ness. Live "stage" frames stop applying once true. */
  terminal: boolean;
  /** Called when a node is selected, by click or by a cold-load #stage-<id> hash. */
  onSelectStage?: (stageId: string) => void;
}

const STATUS_LABELS: Record<string, string> = {
  SUCCESS: "Passed",
  UNSTABLE: "Unstable",
  FAILURE: "Failed",
  ABORTED: "Aborted",
  NOT_BUILT: "Not built",
  SKIPPED: "Skipped",
  NOT_EXECUTED: "Not executed",
  PAUSED: "Paused",
};

const STATUS_TONE: Record<string, "ok" | "bad" | "warn" | "idle"> = {
  SUCCESS: "ok",
  UNSTABLE: "warn",
  FAILURE: "bad",
  ABORTED: "idle",
  NOT_BUILT: "idle",
  SKIPPED: "idle",
  NOT_EXECUTED: "idle",
  PAUSED: "warn",
};

interface StatusDescription {
  label: string;
  tone: "ok" | "bad" | "warn" | "idle";
  running: boolean;
}

/**
 * Reads the record store's stage status as an open set. A known value gets
 * its mockup label and tone; the live status draws with the running ring;
 * an empty status is a stage with no row yet; any other value keeps the
 * raw string as its label, so the accessible name never hides the data.
 */
export function describeStageStatus(status: string | undefined): StatusDescription {
  if (!status) return { label: "Queued", tone: "idle", running: false };
  if (status === "IN_PROGRESS") return { label: "In progress", tone: "idle", running: true };
  const label = STATUS_LABELS[status];
  if (label) return { label, tone: STATUS_TONE[status], running: false };
  return { label: status, tone: "idle", running: false };
}

const LARGE_GRAPH_THRESHOLD = 200;
const COLUMN_W = 160;
const ROW_H = 64;
const NESTED_STEP = 20;
const NODE_R = 15;
const PAD_X = COLUMN_W / 2;
const PAD_Y = 40;
const MAX_LABEL_CHARS = 22;

/**
 * Shortens a stage name to fit one column. The full name stays in the
 * node's title and accessible name.
 */
export function truncateLabel(name: string): string {
  // Array.from splits by code point, so a surrogate pair is never cut in two.
  const characters = Array.from(name);
  return characters.length > MAX_LABEL_CHARS
    ? `${characters.slice(0, MAX_LABEL_CHARS - 1).join("")}…`
    : name;
}

/** One stage rendered in the indented nested list, at its indentation depth. */
interface NestedEntry {
  stage: RunStage;
  depth: number;
}

interface TopNode {
  stage: RunStage;
  column: number;
  row: number;
  rowsInColumn: number;
  nested: NestedEntry[];
}

/** Groups every stage by its parentId; stages with no parent are the trunk. */
function groupByParent(stages: RunStage[]): Map<string, RunStage[]> {
  const byParent = new Map<string, RunStage[]>();
  for (const s of stages) {
    if (!s.parentId) continue;
    const list = byParent.get(s.parentId) ?? [];
    list.push(s);
    byParent.set(s.parentId, list);
  }
  return byParent;
}

/** True when every child of parentId shares nodeType "parallel" — a branch bundle. */
function formsBundle(byParent: Map<string, RunStage[]>, parentId: string): boolean {
  const kids = byParent.get(parentId) ?? [];
  return kids.length > 0 && kids.every((k) => k.nodeType === "parallel");
}

/**
 * Flattens a node's descendants that are not themselves a branch bundle,
 * for the indented nested list, each carrying how many levels deep it sits.
 */
function flattenNested(byParent: Map<string, RunStage[]>, parentId: string, depth = 1): NestedEntry[] {
  const kids = byParent.get(parentId) ?? [];
  if (formsBundle(byParent, parentId)) return [];
  const out: NestedEntry[] = [];
  for (const k of kids) {
    out.push({ stage: k, depth });
    out.push(...flattenNested(byParent, k.stageId, depth + 1));
  }
  return out;
}

/**
 * byStart orders stages by start time, then by stage id. The trunk and the
 * branches of a parallel bundle both use it, so the branches render in the
 * order the pipeline starts them, not in name order. It compares parsed
 * times, because an RFC3339 value can carry a fraction of any length. A
 * stage with no start time sorts after every started stage. Jenkins stage
 * ids are flow node numbers, so a numeric id compares as a number.
 */
function byStart(a: RunStage, b: RunStage): number {
  const at = startTime(a);
  const bt = startTime(b);
  if (at !== bt) {
    if (at === null) return 1;
    if (bt === null) return -1;
    return at - bt;
  }
  return a.stageId.localeCompare(b.stageId, undefined, { numeric: true });
}

function startTime(s: RunStage): number | null {
  if (!s.startedAt) return null;
  const t = Date.parse(s.startedAt);
  return Number.isNaN(t) ? null : t;
}

/**
 * Lays out the stage set left to right: a trunk stage forms its own column;
 * a parallel branch bundle stacks vertically at the next column; a stage
 * that is not part of a bundle renders nested under its parent instead of
 * opening a new column.
 */
function buildColumns(
  stages: RunStage[],
  byParent: Map<string, RunStage[]>,
  largeMode: boolean,
  expandedTrunkId: string | null,
): TopNode[][] {
  const trunk = stages.filter((s) => !s.parentId).sort(byStart);

  const columns: TopNode[][] = [];
  let col = 0;
  for (const t of trunk) {
    const expandThis = !largeMode || t.stageId === expandedTrunkId;
    const bundle = expandThis && formsBundle(byParent, t.stageId);
    const trunkNode: TopNode = {
      stage: t,
      column: col,
      row: 0,
      rowsInColumn: 1,
      nested: expandThis && !bundle ? flattenNested(byParent, t.stageId) : [],
    };
    columns.push([trunkNode]);
    col++;
    if (bundle) {
      const kids = (byParent.get(t.stageId) ?? []).slice().sort(byStart);
      const bundleNodes: TopNode[] = kids.map((k, i) => ({
        stage: k,
        column: col,
        row: i,
        rowsInColumn: kids.length,
        nested: flattenNested(byParent, k.stageId),
      }));
      columns.push(bundleNodes);
      col++;
    }
  }
  return columns;
}

/** Centers each column on centerY, so a tall bundle stays inside the viewBox. */
function nodeCenter(n: TopNode, centerY: number): { x: number; y: number } {
  const x = PAD_X + n.column * COLUMN_W;
  const y = centerY + (n.row - (n.rowsInColumn - 1) / 2) * ROW_H;
  return { x, y };
}

/** Applies one live "stage" frame onto the merged stage list: add or update by stageId. */
function mergeStageEvent(prev: RunStage[], ev: RunLogStageEvent): RunStage[] {
  const idx = prev.findIndex((s) => s.stageId === ev.stageId);
  if (idx === -1) {
    return [...prev, { stageId: ev.stageId, name: ev.name, status: ev.status }];
  }
  const next = prev.slice();
  next[idx] = { ...next[idx], status: ev.status, name: ev.name || next[idx].name };
  return next;
}

export function StageGraph({ stages, lastEvent, terminal, onSelectStage }: StageGraphProps) {
  const [merged, setMerged] = useState<RunStage[]>(stages);
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set());
  const [expandedTrunkId, setExpandedTrunkId] = useState<string | null>(null);

  // The stage list is read from the run record on load; a later prop
  // identity change means a fresh GetRun result and resets the merge.
  useEffect(() => {
    setMerged(stages);
  }, [stages]);

  useEffect(() => {
    if (!lastEvent || lastEvent.type !== "stage" || terminal) return;
    setMerged((prev) => mergeStageEvent(prev, lastEvent.data as RunLogStageEvent));
  }, [lastEvent, terminal]);

  // A cold load of a build URL whose fragment names a stage opens with that
  // stage's log group in view.
  useEffect(() => {
    const m = /^#stage-(.+)$/.exec(window.location.hash);
    if (m) onSelectStage?.(decodeURIComponent(m[1]));
    // Runs once, at mount, reading the URL the page opened with.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const byParent = useMemo(() => groupByParent(merged), [merged]);
  const largeMode = merged.length > LARGE_GRAPH_THRESHOLD;
  const columns = useMemo(
    () => buildColumns(merged, byParent, largeMode, expandedTrunkId),
    [merged, byParent, largeMode, expandedTrunkId],
  );
  const topNodes = useMemo(() => columns.flat(), [columns]);

  const width = PAD_X * 2 + Math.max(0, columns.length - 1) * COLUMN_W;
  const maxRows = Math.max(1, ...topNodes.map((n) => n.rowsInColumn));
  const height = PAD_Y * 2 + Math.max(0, maxRows - 1) * ROW_H;
  const centerY = height / 2;

  const connectors = useMemo(() => {
    const paths: { id: string; d: string; completed: boolean }[] = [];
    for (let i = 0; i < columns.length - 1; i++) {
      for (const a of columns[i]) {
        for (const b of columns[i + 1]) {
          const p1 = nodeCenter(a, centerY);
          const p2 = nodeCenter(b, centerY);
          const midX = (p1.x + p2.x) / 2;
          paths.push({
            id: `${a.stage.stageId}-${b.stage.stageId}`,
            d: `M ${p1.x} ${p1.y} C ${midX} ${p1.y}, ${midX} ${p2.y}, ${p2.x} ${p2.y}`,
            completed: b.stage.status === "SUCCESS",
          });
        }
      }
    }
    return paths;
  }, [columns, centerY]);

  const toggleNested = (stageId: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(stageId)) next.delete(stageId);
      else next.add(stageId);
      return next;
    });
  };

  const select = (stageId: string) => onSelectStage?.(stageId);

  if (merged.length === 0) {
    return (
      <div className={styles.wrap}>
        <p className={styles.empty}>This run has no stages.</p>
      </div>
    );
  }

  // A trunk node's own children are hidden while it is not the one expanded
  // bundle in trunk-only mode, but the control to expand it must still show.
  const hasHiddenChildren = (stageId: string): boolean =>
    largeMode && stageId !== expandedTrunkId && (byParent.get(stageId)?.length ?? 0) > 0;

  return (
    <div className={styles.wrap}>
      <div className={styles.scroll}>
        <svg
          className={styles.svg}
          width={width}
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          role="img"
          aria-label="Pipeline stages"
        >
          {connectors.map((c) => (
            <path
              key={c.id}
              d={c.d}
              className={c.completed ? styles.connOk : styles.connPlain}
            />
          ))}
          {topNodes.map((n) => {
            const desc = describeStageStatus(n.stage.status);
            const { x, y } = nodeCenter(n, centerY);
            return (
              <g key={n.stage.stageId} transform={`translate(${x},${y})`}>
                <a
                  href={`#stage-${n.stage.stageId}`}
                  id={`node-stage-${n.stage.stageId}`}
                  className={styles.nodeLink}
                  aria-label={`${n.stage.name} — ${desc.label}`}
                  onClick={(e) => {
                    e.preventDefault();
                    select(n.stage.stageId);
                  }}
                >
                  <title>{`${n.stage.name} — ${desc.label}`}</title>
                  {desc.running && <circle r={NODE_R + 4} className={styles.ring} />}
                  <circle r={NODE_R} className={styles[`tone_${desc.tone}`]} />
                  <text className={styles.label} y={NODE_R + 14} textAnchor="middle">
                    {truncateLabel(n.stage.name)}
                  </text>
                </a>
                {hasHiddenChildren(n.stage.stageId) && (
                  <text
                    role="button"
                    tabIndex={0}
                    className={styles.expandTrunk}
                    x={0}
                    y={-(NODE_R + 8)}
                    textAnchor="middle"
                    aria-label={`Expand ${n.stage.name}`}
                    onClick={() => setExpandedTrunkId(n.stage.stageId)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") setExpandedTrunkId(n.stage.stageId);
                    }}
                  >
                    Expand
                  </text>
                )}
              </g>
            );
          })}
        </svg>
      </div>
      {topNodes
        .filter((n) => n.nested.length > 0)
        .map((n) => {
          const open = !collapsed.has(n.stage.stageId);
          const listId = `nested-${n.stage.stageId}`;
          return (
            <section key={`${n.stage.stageId}-nested`} className={styles.nestedGroup}>
              <button
                type="button"
                className={styles.nestedHead}
                aria-expanded={open}
                aria-controls={open ? listId : undefined}
                aria-label={`Toggle nested stages of ${n.stage.name}`}
                onClick={() => toggleNested(n.stage.stageId)}
              >
                <span aria-hidden="true">{open ? "▾" : "▸"}</span> {n.stage.name}
              </button>
              {open && (
                <ul id={listId} className={styles.nestedList} aria-label={`${n.stage.name} nested stages`}>
                  {n.nested.map(({ stage, depth }) => {
                    const desc = describeStageStatus(stage.status);
                    return (
                      <li key={stage.stageId} className={styles.nestedItem} style={{ paddingLeft: depth * NESTED_STEP }}>
                        <a
                          href={`#stage-${stage.stageId}`}
                          id={`node-stage-${stage.stageId}`}
                          title={`${stage.name} — ${desc.label}`}
                          aria-label={`${stage.name} — ${desc.label}`}
                          onClick={(e) => {
                            e.preventDefault();
                            select(stage.stageId);
                          }}
                        >
                          {stage.name}
                        </a>
                        <span className={styles[`nestedTone_${desc.tone}`]}>{desc.label}</span>
                      </li>
                    );
                  })}
                </ul>
              )}
            </section>
          );
        })}
    </div>
  );
}
