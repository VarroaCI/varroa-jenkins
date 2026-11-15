import type { ActivityEvent, FleetRunGroup, FleetRunRow, FleetTriggerKind } from "../../types";
import { repeatKey } from "../../components/activityTimeline.util";
import { runActorText } from "../../components/runs/runRowText";
import { stripTenantFolder } from "../pipelines/jobPath";
import { hhmm } from "./attentionItems";
import { shortDuration } from "./dayText";

/** HUMAN_TRIGGERS are the triggers that a person starts. A single run with one of them keeps its own row. */
export const HUMAN_TRIGGERS: FleetTriggerKind[] = ["manual", "replay", "restart"];

/** FEED_ROWS is the number of timed rows the feed shows before "Show all". */
export const FEED_ROWS = 8;

/** FOLD_ROWS is the number of job folds the feed shows before "Show all jobs". */
export const FOLD_ROWS = 5;

/** TimedRunRow is one run that keeps its own row. Its group holds one run. */
export interface TimedRunRow {
  kind: "run";
  key: string;
  /** The run's end time, or its creation time, as an ISO string. It orders the rows. */
  atIso: string;
  group: FleetRunGroup;
}

/** TimedEventRow is one activity event. A repeat of the same event folds into one row. */
export interface TimedEventRow {
  kind: "event";
  key: string;
  atIso: string;
  event: ActivityEvent;
  repeats: number;
}

/** FoldRow is the job fold: two or more runs of one job in the window. */
export interface FoldRow {
  kind: "fold";
  key: string;
  group: FleetRunGroup;
}

/** OtherRow folds the single passes of machine triggers. Each group holds one run. */
export interface OtherRow {
  kind: "other";
  key: string;
  groups: FleetRunGroup[];
  runs: number;
}

/** DayFeed holds the rows of the feed. Timed rows come first, newest first. Job folds follow, then the other row. */
export interface DayFeed {
  timed: (TimedRunRow | TimedEventRow)[];
  folds: FoldRow[];
  other?: OtherRow;
}

const RESULT_WORDS: Record<string, string> = {
  SUCCESS: "passed",
  FAILURE: "failed",
  UNSTABLE: "unstable",
  ABORTED: "aborted",
  INTERRUPTED: "interrupted",
  NOT_BUILT: "not built",
};

/** resultWord names a finished result in the words of the feed. */
export function resultWord(result?: string): string {
  if (!result) return "running";
  return RESULT_WORDS[result] ?? result.toLowerCase().replace(/_/g, " ");
}

/** isHumanRun reports whether a person started the run. */
export function isHumanRun(r: FleetRunRow): boolean {
  return HUMAN_TRIGGERS.includes(r.trigger.kind);
}

/** jobName names a job without its tenant folder. */
function jobName(g: FleetRunGroup): string {
  return stripTenantFolder(g.tenantFolder, g.jobPath) || g.jobPath;
}

/** groupTitle names a job fold, or one run with its number and result. */
export function groupTitle(g: FleetRunGroup): string {
  const job = `${g.tenant} / ${jobName(g)}`;
  if (g.runs >= 2) return `${job} ran ${g.runs} times`;
  const build = g.newest.buildNumber !== undefined ? ` #${g.newest.buildNumber}` : "";
  return `${job}${build} ${resultWord(g.newest.result)}`;
}

/** groupDetail names the actor and duration of one run, or the pass and failure counts of a fold. */
export function groupDetail(g: FleetRunGroup): string {
  if (g.runs >= 2) return foldDetail(g);
  const parts: string[] = [];
  const actor = runActorText(g.newest);
  if (actor) parts.push(`Started by ${actor}`);
  if (g.newest.durationMs !== undefined) parts.push(shortDuration(g.newest.durationMs));
  return parts.join(" · ");
}

/** foldDetail names the counts of a job fold and the time it spans. */
function foldDetail(g: FleetRunGroup): string {
  const other = g.runs - g.passed - g.failed;
  const from = hhmm(g.firstAt);
  const to = hhmm(g.newest.endedAt ?? g.newest.createdAt);
  if (g.failed === 0 && other === 0) return `All passed · ${from} to ${to}`;
  const parts = [`${g.failed} failed`, `${g.passed} passed`];
  if (other > 0) parts.push(`${other} other`);
  parts.push(`${from} to ${to}`);
  return parts.join(" · ");
}

/** tenantCounts names the runs of each tenant, the largest count first, then by name. */
export function tenantCounts(groups: FleetRunGroup[]): string {
  const counts = new Map<string, number>();
  for (const g of groups) counts.set(g.tenant, (counts.get(g.tenant) ?? 0) + g.runs);
  return [...counts.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([tenant, n]) => `${tenant} (${n})`)
    .join(", ");
}

/** startOfLocalDay answers local midnight of the day that holds `now`. */
export function startOfLocalDay(now: number): Date {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  return d;
}

/** eventTitle is the message of an activity event. */
export function eventTitle(e: ActivityEvent): string {
  return e.message;
}

/** eventDetail names the controller and the actor of an event, when the event has them. */
export function eventDetail(e: ActivityEvent): string | undefined {
  const parts = [e.controller, e.actor].filter((p): p is string => Boolean(p));
  return parts.length > 0 ? parts.join(" · ") : undefined;
}

/**
 * buildDayFeed orders the runs and the events of the day. A group of two or more
 * runs is a job fold. A single run keeps its own row when a person started it or
 * it did not pass. The single passes of machine triggers fold into one other row
 * when there are two or more. Repeats of one event fold into one row across the
 * whole day, because the repeat key holds the message. The events keep the order
 * the server sent, newest first.
 */
export function buildDayFeed(groups: FleetRunGroup[], events: ActivityEvent[]): DayFeed {
  const timed: (TimedRunRow | TimedEventRow)[] = [];
  const folds: FoldRow[] = [];
  const others: FleetRunGroup[] = [];

  for (const g of groups) {
    const key = `${g.cluster}/${g.tenant}/${g.jobPath}`;
    if (g.runs >= 2) {
      folds.push({ kind: "fold", key: `fold:${key}`, group: g });
    } else if (isHumanRun(g.newest) || g.passed === 0) {
      timed.push(runRow(g));
    } else {
      others.push(g);
    }
  }
  let other: OtherRow | undefined;
  if (others.length === 1) {
    timed.push(runRow(others[0]));
  } else if (others.length >= 2) {
    other = { kind: "other", key: "other", groups: others, runs: others.length };
  }

  const byKey = new Map<string, TimedEventRow>();
  events.forEach((e, i) => {
    const k = repeatKey(e);
    const seen = byKey.get(k);
    if (seen) {
      seen.repeats += 1;
      return;
    }
    const row: TimedEventRow = { kind: "event", key: `event:${i}:${e.timestamp}`, atIso: e.timestamp, event: e, repeats: 1 };
    byKey.set(k, row);
    timed.push(row);
  });

  const at = (r: TimedRunRow | TimedEventRow) => Date.parse(r.atIso);
  timed.sort((a, b) => at(b) - at(a));

  folds.sort((a, b) => b.group.runs - a.group.runs || a.group.tenant.localeCompare(b.group.tenant) || a.group.jobPath.localeCompare(b.group.jobPath));

  return { timed, folds, other };
}

/**
 * activityHref links to the activity view of one controller, from the start of
 * the day. It leaves out the cluster when the event has none.
 */
export function activityHref(cluster: string | undefined, namespace: string | undefined, controller: string | undefined, since: string): string {
  const qs = new URLSearchParams();
  if (cluster) qs.set("cluster", cluster);
  qs.set("controller", `${namespace ?? ""}/${controller ?? ""}`);
  qs.set("range", "custom");
  qs.set("start", since);
  return `/activity?${qs.toString()}`;
}

/** runRow makes the timed row of one run. */
function runRow(g: FleetRunGroup): TimedRunRow {
  return {
    kind: "run",
    key: `run:${g.newest.runId}`,
    atIso: g.newest.endedAt ?? g.newest.createdAt,
    group: g,
  };
}
