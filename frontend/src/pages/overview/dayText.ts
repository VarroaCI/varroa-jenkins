import type { PipelinesOverviewHour, PipelinesOverviewRunningRun, PipelinesOverviewScheduledRun, PipelinesOverviewTotals } from "../../types";
import { formatDuration } from "../pipelines/JobTree";
import { stripTenantFolder } from "../pipelines/jobPath";
import { hhmm } from "./attentionItems";

/**
 * shortDuration is formatDuration with a zero second part dropped, so "2m 0s"
 * reads "2m". Other spans keep formatDuration's text.
 */
export function shortDuration(ms: number | undefined): string {
  return formatDuration(ms).replace(/ 0s$/, "");
}

/** TotalsParts are the counters of the totals line. */
export interface TotalsParts {
  runs: number;
  passed: number;
  failed: number;
  median: string;
}

/** totalsParts answers the totals line, or undefined when no run happened in the window. */
export function totalsParts(t: PipelinesOverviewTotals): TotalsParts | undefined {
  if (t.runs24h === 0) return undefined;
  return { runs: t.runs24h, passed: t.passed, failed: t.failed, median: shortDuration(t.medianDurationMs) };
}

/** failedHoursLabel names the hours that had a failure, for the bar chart's label. */
export function failedHoursLabel(hourly: PipelinesOverviewHour[]): string {
  const failed = hourly.filter((h) => h.failed > 0).map((h) => hhmm(h.start));
  const head = "Runs per hour, last 24 hours.";
  return failed.length > 0 ? `${head} Failures at ${failed.join(", ")}.` : `${head} No failures.`;
}

/** RunningParts split the running line: a run name, which reads bold, and the text after it. */
export interface RunningParts {
  name: string;
  detail: string;
}

/**
 * runningParts names the first run that executes now, with its stage and its
 * elapsed time. With no run it answers an empty name and "Nothing is running.".
 */
export function runningParts(running: PipelinesOverviewRunningRun[], now: number): RunningParts {
  if (running.length === 0) return { name: "", detail: "Nothing is running." };
  const first = running[0];
  const r = first.run;
  const job = stripTenantFolder(r.tenantFolder, r.jobPath ?? "") || r.jobPath || "";
  const build = r.buildNumber !== undefined ? ` #${r.buildNumber}` : "";
  const parts: string[] = [];
  if (first.stage) parts.push(`${first.stage} stage`);
  if (r.startedAt) parts.push(shortDuration(Math.max(0, now - Date.parse(r.startedAt))));
  let detail = parts.map((p) => ` · ${p}`).join("");
  if (running.length > 1) detail += ` and ${running.length - 1} more`;
  return { name: `${r.tenant} / ${job}${build}`, detail };
}

/** nextScheduledLine names the queue length and the next cron run. */
export function nextScheduledLine(queued: number, next: PipelinesOverviewScheduledRun | undefined): string {
  if (!next) return `${queued} queued · no scheduled run`;
  const job = stripTenantFolder(next.tenantFolder, next.jobPath) || next.jobPath;
  return `${queued} queued · next scheduled: ${next.tenant} / ${job} at ${hhmm(next.at)}`;
}

/** JenkinsCounts are the controller counts of the Jenkins row. */
export interface JenkinsCounts {
  connected: number;
  total: number;
  asleep: number;
  stopped: number;
}

/** jenkinsLine names the connected controllers, and the asleep and stopped ones when any exist. */
export function jenkinsLine(c: JenkinsCounts): string {
  let text = `${c.connected} of ${c.total} connected`;
  if (c.asleep > 0) text += ` · ${c.asleep} asleep`;
  if (c.stopped > 0) text += ` · ${c.stopped} stopped`;
  return text;
}
