import type { FleetRunRow } from "../../types";
import { age } from "../activityTimeline.util";
import { formatDuration } from "../../pages/pipelines/JobTree";

/** RunGlyph names the state a run row shows as its status mark. */
export type RunGlyph =
  | "success"
  | "failure"
  | "unstable"
  | "aborted"
  | "interrupted"
  | "running"
  | "queued"
  | "waiting"
  | "other";

/** runGlyph answers the status mark of a run. A pending input outranks the status. */
export function runGlyph(r: FleetRunRow): RunGlyph {
  if (r.pendingInput) return "waiting";
  if (r.status === "QUEUED") return "queued";
  if (r.status === "RUNNING") return "running";
  switch (r.result) {
    case "SUCCESS":
      return "success";
    case "FAILURE":
    case "TRIGGER_FAILED":
      return "failure";
    case "UNSTABLE":
      return "unstable";
    case "ABORTED":
      return "aborted";
    case "INTERRUPTED":
      return "interrupted";
    default:
      return "other";
  }
}

/** runGlyphLabel names a glyph for assistive technology and for the reader. */
export function runGlyphLabel(g: RunGlyph): string {
  switch (g) {
    case "success":
      return "Success";
    case "failure":
      return "Failure";
    case "unstable":
      return "Unstable";
    case "aborted":
      return "Aborted";
    case "interrupted":
      return "Interrupted";
    case "running":
      return "Running";
    case "queued":
      return "Queued";
    case "waiting":
      return "Waiting for input";
    default:
      return "Other";
  }
}

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
