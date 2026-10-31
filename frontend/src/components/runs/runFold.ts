import type { FleetRunRow } from "../../types";
import { runJobName, titleIsJob } from "./runRowText";

/** FOLD_MAX is the most runs that one fold holds. A longer run of matching rows starts a new fold. */
export const FOLD_MAX = 200;

/** FeedItem is one row of the feed: one run, or a fold of consecutive matching runs. */
export type FeedItem =
  | { kind: "run"; run: FleetRunRow }
  | { kind: "fold"; id: string; runs: FleetRunRow[] };

const SEP = "\u001F";

/** RESULT_TEXT names the outcome of a fold in its second line. */
const RESULT_TEXT: Record<string, string> = {
  SUCCESS: "all passed",
  FAILURE: "all failed",
  UNSTABLE: "all unstable",
  ABORTED: "all aborted",
  INTERRUPTED: "all interrupted",
  NOT_BUILT: "none built",
};

/**
 * foldKey names the runs that may fold together. A run has no key unless it is
 * COMPLETED, waits for no input, has a result, and has no commit subject.
 * A commit subject names one change, so such a run always stays a row.
 * The branch belongs to the key, because a fold row shows one branch chip.
 */
export function foldKey(r: FleetRunRow): string | undefined {
  if (r.status !== "COMPLETED" || r.pendingInput || !r.result || !titleIsJob(r)) return undefined;
  return [r.cluster, r.tenant, r.jobPath ?? "", r.branch ?? "", r.trigger.kind, r.actor ?? "", r.result].join(SEP);
}

/**
 * foldRuns folds each run of two or more consecutive rows that share a key
 * into one fold item. The rows keep their order, newest first. A row with no
 * key, or a run of one, stays a run item. A fold holds at most FOLD_MAX runs.
 * A fold id counts the folds of one key, so the id stays when older runs load.
 */
export function foldRuns(rows: readonly FleetRunRow[]): FeedItem[] {
  const out: FeedItem[] = [];
  const ordinals = new Map<string, number>();
  let group: FleetRunRow[] = [];
  let groupKey: string | undefined;

  const flush = () => {
    if (group.length === 1) {
      out.push({ kind: "run", run: group[0] });
    } else if (group.length > 1 && groupKey !== undefined) {
      const ordinal = ordinals.get(groupKey) ?? 0;
      ordinals.set(groupKey, ordinal + 1);
      out.push({ kind: "fold", id: `${groupKey}#${ordinal}`, runs: group });
    }
    group = [];
    groupKey = undefined;
  };

  for (const row of rows) {
    const key = foldKey(row);
    if (key !== undefined && key === groupKey && group.length < FOLD_MAX) {
      group.push(row);
      continue;
    }
    flush();
    if (key === undefined) {
      out.push({ kind: "run", run: row });
    } else {
      groupKey = key;
      group = [row];
    }
  }
  flush();
  return out;
}

/** foldTitle answers the link text of a fold: its job, and the run count. */
export function foldTitle(runs: readonly FleetRunRow[]): string {
  return `${runJobName(runs[0])} × ${runs.length}`;
}

/**
 * foldSecondLine answers `<tenant> · <outcome> · #<first>–#<last>`. The build
 * range is left out when a run of the fold has no build number.
 */
export function foldSecondLine(runs: readonly FleetRunRow[]): string {
  const newest = runs[0];
  const outcome = RESULT_TEXT[newest.result ?? ""] ?? `all ${(newest.result ?? "").toLowerCase()}`;
  const numbers = runs.map((r) => r.buildNumber).filter((n): n is number => n !== undefined);
  const range =
    numbers.length === runs.length ? ` · #${Math.min(...numbers)}–#${Math.max(...numbers)}` : "";
  return `${newest.tenant} · ${outcome}${range}`;
}
