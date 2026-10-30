import type { FleetRunRow } from "../../types";
import { age } from "../activityTimeline.util";
import { formatDuration } from "../../pages/pipelines/JobTree";

function elapsedSince(ts: string | undefined, now: number): number | undefined {
  if (!ts) return undefined;
  const parsed = Date.parse(ts);
  if (Number.isNaN(parsed)) return undefined;
  return now - parsed;
}

/**
 * runTiming answers the timing text of a run: its queue state, its elapsed
 * time while it runs, its wait for input, its interrupted reason, or its age
 * and duration once it ends.
 */
export function runTiming(r: FleetRunRow, now: number): string {
  if (r.pendingInput) {
    const waited = elapsedSince(r.startedAt ?? r.createdAt, now);
    return waited === undefined ? "waiting" : `waiting ${formatDuration(waited)}`;
  }
  if (r.status === "QUEUED") return "queued";
  if (r.status === "RUNNING") {
    const elapsed = elapsedSince(r.startedAt, now);
    return elapsed === undefined ? "running" : `running · ${formatDuration(elapsed)}`;
  }
  if (r.result === "INTERRUPTED") {
    return r.interruptedReason ? `interrupted · ${r.interruptedReason}` : "interrupted";
  }
  const started = r.startedAt ?? r.createdAt;
  return `${age(started, { now })} · ${formatDuration(r.durationMs)}`;
}

/** runSecondLine answers `<tenant> / <job> #<n>: <trigger> by <actor>`. */
export function runSecondLine(r: FleetRunRow): string {
  const build = r.buildNumber !== undefined ? ` #${r.buildNumber}` : "";
  const by = r.actor ? ` by ${r.actor}` : "";
  return `${r.tenant} / ${r.jobPath ?? ""}${build}: ${r.trigger.label}${by}`;
}

/** runHref answers the build page of one run. The job path is one encoded segment. */
export function runHref(tenant: string, jobPath: string, runId: string): string {
  return `/pipelines/${encodeURIComponent(tenant)}/${encodeURIComponent(jobPath)}/${encodeURIComponent(runId)}`;
}
