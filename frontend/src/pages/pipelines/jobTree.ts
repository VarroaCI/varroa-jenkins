import type { PipelineSummary, RunningRun } from "../../types";
import { stripTenantFolder } from "./jobPath";

/**
 * A node of the tenant home's folder tree. The kind set is complete: a
 * "folder" node is a path segment that no item declares, a "multibranch"
 * node is an item the API reports as multibranch, an "organizationFolder"
 * node is an item the API reports as an organization folder, and a "job"
 * node is any other item: a pipeline, a branch, or an unknown job.
 *
 * A "folder", a "multibranch", and an "organizationFolder" node fold. A
 * "job" node does not.
 */
export type JobNodeKind = "folder" | "job" | "multibranch" | "organizationFolder";

export interface JobTreeNode {
  /** The node's own path segment. */
  name: string;
  /** The folder-relative path of the node. */
  path: string;
  /** The folder-relative depth. A top-level node is 0. */
  depth: number;
  kind: JobNodeKind;
  /** The child nodes, already sorted. */
  children: JobTreeNode[];
  /** The number of "job" nodes at or below this node. */
  jobCount: number;
  /** The number of running builds whose job path sits at or below. */
  runningCount: number;
  /** For a "multibranch" node: the number of its direct "job" children. */
  branchCount: number;
  /**
   * For an "organizationFolder" node: the number of its direct "multibranch"
   * children, which are its repositories.
   */
  repositoryCount: number;
  /** For a "job" node: the row the table renders. */
  summary?: PipelineSummary;
}

/** A node still under construction, before its counts are computed. */
interface Draft {
  name: string;
  path: string;
  depth: number;
  kind: JobNodeKind;
  children: Map<string, Draft>;
  summary?: PipelineSummary;
}

function newDraft(name: string, path: string, depth: number): Draft {
  return { name, path, depth, kind: "folder", children: new Map() };
}

/** nodeKindFor maps an API item kind onto a node kind. */
function nodeKindFor(item: PipelineSummary): JobNodeKind {
  switch (item.kind) {
    case "multibranch":
      return "multibranch";
    case "organizationFolder":
      return "organizationFolder";
    default:
      return "job";
  }
}

/**
 * buildJobTree builds the folder tree the tenant home renders. It strips
 * `tenantFolder` from every job path first, so a controller tenant, whose
 * folder is empty, and a placed tenant both produce folder-relative nodes.
 *
 * An item whose job path equals `tenantFolder` leaves an empty remainder.
 * That item is the container, not a job, and produces no node.
 */
export function buildJobTree(
  items: PipelineSummary[],
  runningNow: RunningRun[],
  tenantFolder: string,
): JobTreeNode[] {
  const folder = tenantFolder ?? "";
  const roots = new Map<string, Draft>();

  for (const item of items) {
    const relative = stripTenantFolder(folder, item.jobPath);
    if (relative === "") continue;

    const segments = relative.split("/");
    let level = roots;
    let prefix = "";
    let node: Draft | undefined;
    segments.forEach((segment, index) => {
      prefix = index === 0 ? segment : `${prefix}/${segment}`;
      let child = level.get(segment);
      if (!child) {
        child = newDraft(segment, prefix, index);
        level.set(segment, child);
      }
      node = child;
      level = child.children;
    });
    if (!node) continue;

    // The declaring item decides the node's kind, whichever order the
    // items arrive in. A node that no item declares stays a folder.
    node.kind = nodeKindFor(item);
    node.summary = item;
  }

  const runningPaths = runningNow.map((run) => stripTenantFolder(folder, run.jobPath));
  return [...roots.values()]
    .map((draft) => finish(draft, runningPaths))
    .sort(compareNodes);
}

/** finish converts one draft and its descendants into finished nodes. */
function finish(draft: Draft, runningPaths: string[]): JobTreeNode {
  const children = [...draft.children.values()]
    .map((child) => finish(child, runningPaths))
    .sort(compareNodes);

  const ownJob = draft.kind === "job" ? 1 : 0;
  const jobCount = children.reduce((sum, child) => sum + child.jobCount, ownJob);
  const runningCount = runningPaths.filter((path) => isAtOrBelow(draft.path, path)).length;
  const branchCount =
    draft.kind === "multibranch" ? children.filter((child) => child.kind === "job").length : 0;
  const repositoryCount =
    draft.kind === "organizationFolder"
      ? children.filter((child) => child.kind === "multibranch").length
      : 0;

  return {
    name: draft.name,
    path: draft.path,
    depth: draft.depth,
    kind: draft.kind,
    children,
    jobCount,
    runningCount,
    branchCount,
    repositoryCount,
    summary: draft.summary,
  };
}

/** isAtOrBelow reports whether path names nodePath itself or a descendant. */
function isAtOrBelow(nodePath: string, path: string): boolean {
  return path === nodePath || path.startsWith(`${nodePath}/`);
}

/**
 * compareNodes orders a level: a foldable node sorts before a job node, and
 * inside each group the order is alphabetical by the node's own segment.
 */
function compareNodes(a: JobTreeNode, b: JobTreeNode): number {
  const aFoldable = isFoldable(a) ? 0 : 1;
  const bFoldable = isFoldable(b) ? 0 : 1;
  if (aFoldable !== bFoldable) return aFoldable - bFoldable;
  return a.name.localeCompare(b.name);
}

/** isFoldable reports whether a node carries a disclosure control. */
export function isFoldable(node: JobTreeNode): boolean {
  return node.kind === "folder" || node.kind === "multibranch" || node.kind === "organizationFolder";
}

/**
 * defaultOpenPaths returns the paths a first load unfolds: every foldable
 * node whose depth is below `levels`. A multibranch node counts as a level,
 * because it folds like a folder. `levels` is one-based and `depth` is
 * zero-based, so `levels` of 2 returns depth 0 and depth 1.
 */
export function defaultOpenPaths(tree: JobTreeNode[], levels: number): string[] {
  const open: string[] = [];
  const walk = (nodes: JobTreeNode[]) => {
    for (const node of nodes) {
      if (!isFoldable(node)) continue;
      if (node.depth < levels) open.push(node.path);
      walk(node.children);
    }
  };
  walk(tree);
  return open;
}

/**
 * visibleRows flattens the tree into the rows the table renders. A folded
 * node's descendants are absent from the result, so they never reach the
 * page and no reader can find them.
 */
export function visibleRows(tree: JobTreeNode[], open: ReadonlySet<string>): JobTreeNode[] {
  const rows: JobTreeNode[] = [];
  const walk = (nodes: JobTreeNode[]) => {
    for (const node of nodes) {
      rows.push(node);
      if (isFoldable(node) && open.has(node.path)) walk(node.children);
    }
  };
  walk(tree);
  return rows;
}
