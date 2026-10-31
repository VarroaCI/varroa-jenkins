import type { FleetRunRow } from "../../types";
import { age } from "../activityTimeline.util";
import { formatDuration } from "../../pages/pipelines/JobTree";
import { stripTenantFolder } from "../../pages/pipelines/jobPath";
import { displayActor, isMachineActor } from "./runActor";

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

/** runJobName answers the job path without its tenant folder. */
export function runJobName(r: FleetRunRow): string {
  const path = r.jobPath ?? "";
  return stripTenantFolder(r.tenantFolder, path) || path;
}

/**
 * titleIsJob is true when the run has no commit subject, so its title is
 * its job path. Such a row names the job once, with its build number.
 */
export function titleIsJob(r: FleetRunRow): boolean {
  return r.title === (r.jobPath ?? "");
}

/** runTitle answers the link text of a run: its commit subject, or its job and build number. */
export function runTitle(r: FleetRunRow): string {
  if (!titleIsJob(r)) return r.title;
  const build = r.buildNumber !== undefined ? ` #${r.buildNumber}` : "";
  return `${runJobName(r)}${build}`;
}

/** runActorText answers the actor text of a run, or undefined when the row shows no actor. */
export function runActorText(r: FleetRunRow): string | undefined {
  if (isMachineActor(r.actor)) return undefined;
  return displayActor(r.actor);
}

/**
 * runSecondLine answers `<tenant>: <trigger> by <actor>` when the title names
 * the job. Otherwise it answers `<tenant> / <job> #<n>: <trigger> by <actor>`.
 * The job path is shown without its tenant folder.
 */
export function runSecondLine(r: FleetRunRow): string {
  const actor = runActorText(r);
  const by = actor ? ` by ${actor}` : "";
  if (titleIsJob(r)) return `${r.tenant}: ${r.trigger.label}${by}`;
  const build = r.buildNumber !== undefined ? ` #${r.buildNumber}` : "";
  return `${r.tenant} / ${runJobName(r)}${build}: ${r.trigger.label}${by}`;
}

/** runHref answers the build page of one run. The job path is one encoded segment. */
export function runHref(tenant: string, jobPath: string, runId: string): string {
  return `/pipelines/${encodeURIComponent(tenant)}/${encodeURIComponent(jobPath)}/${encodeURIComponent(runId)}`;
}
