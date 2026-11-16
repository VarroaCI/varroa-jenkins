import type { ClusterEntry, ControllerAttentionKind, PipelinesOverviewFailingJob, PipelinesOverviewWaitingInput } from "../../types";
import type { ControllerListItem } from "../../hooks/useControllers";
import { age } from "../../components/activityTimeline.util";
import { runActorText } from "../../components/runs/runRowText";
import { ATTENTION_LABEL } from "../../components/StatusPill";
import { stripTenantFolder } from "../pipelines/jobPath";
import { plural } from "../../lib/plural";

/** ATTENTION_ROWS is the number of rows the panel shows before "Show all". */
export const ATTENTION_ROWS = 8;

/** CONTROLLER_ATTENTION_ORDER ranks the controller attention kinds, first to last. */
export const CONTROLLER_ATTENTION_ORDER: ControllerAttentionKind[] = [
  "failed",
  "reconcileBlocked",
  "bootFailed",
  "pluginRollFailed",
  "applyFailed",
  "miteUnreachable",
];

/** AttentionItem is one row of the attention panel. Each kind carries the data its row needs. */
export type AttentionItem =
  | { kind: "input"; key: string; waiting: PipelinesOverviewWaitingInput }
  | { kind: "failing"; key: string; job: PipelinesOverviewFailingJob }
  | { kind: "cluster"; key: string; cluster: ClusterEntry }
  | { kind: "controller"; key: string; controller: ControllerListItem }
  | { kind: "updateCenter"; key: string; unservable: number };

/** AttentionSources are the reads the panel combines. A missing read contributes no rows. */
export interface AttentionSources {
  waitingInputs: PipelinesOverviewWaitingInput[];
  failingJobs: PipelinesOverviewFailingJob[];
  clusters: ClusterEntry[];
  controllers: ControllerListItem[];
  /** The update center's unservable plugin count. Zero when the row does not apply. */
  updateCenterUnservable: number;
}

function byTime(a: string, b: string): number {
  return Date.parse(a) - Date.parse(b);
}

/**
 * attentionItems orders the items that need a person. Inputs come first, oldest
 * run first. Failing jobs follow, newest failure first. Stopped clusters come by
 * name, then controllers by attention precedence, then the update center.
 * A healthy, draining, or core cluster is left out.
 */
export function attentionItems(s: AttentionSources): AttentionItem[] {
  const out: AttentionItem[] = [];

  const inputs = [...s.waitingInputs].sort(
    (a, b) => byTime(a.run.createdAt, b.run.createdAt) || a.run.runId.localeCompare(b.run.runId),
  );
  for (const waiting of inputs) out.push({ kind: "input", key: `input:${waiting.run.runId}`, waiting });

  const failing = [...s.failingJobs].sort(
    (a, b) => byTime(b.run.createdAt, a.run.createdAt) || b.run.runId.localeCompare(a.run.runId),
  );
  for (const job of failing) out.push({ kind: "failing", key: `failing:${job.run.runId}`, job });

  const clusters = s.clusters
    .filter((c) => !c.core && !c.healthy && c.state === "active")
    .sort((a, b) => a.name.localeCompare(b.name));
  for (const cluster of clusters) out.push({ kind: "cluster", key: `cluster:${cluster.name}`, cluster });

  const rank = (k: ControllerAttentionKind) => CONTROLLER_ATTENTION_ORDER.indexOf(k);
  const controllers = s.controllers
    .filter((c) => c.attention !== undefined)
    .sort(
      (a, b) =>
        rank(a.attention!.kind) - rank(b.attention!.kind) ||
        a.cluster.localeCompare(b.cluster) ||
        a.namespace.localeCompare(b.namespace) ||
        a.name.localeCompare(b.name),
    );
  for (const controller of controllers) {
    const key = `controller:${controller.cluster}/${controller.namespace}/${controller.name}`;
    out.push({ kind: "controller", key, controller });
  }

  if (s.updateCenterUnservable > 0) {
    out.push({ kind: "updateCenter", key: "updateCenter", unservable: s.updateCenterUnservable });
  }
  return out;
}

/** attentionBreakdown names each kind in the panel header, for example "1 approval · 2 failing jobs". */
export function attentionBreakdown(items: AttentionItem[]): string {
  const count = (k: AttentionItem["kind"]) => items.filter((i) => i.kind === k).length;
  const parts: string[] = [];
  const inputs = count("input");
  const failing = count("failing");
  const clusters = count("cluster");
  const controllers = count("controller");
  if (inputs > 0) parts.push(plural(inputs, "approval", "approvals"));
  if (failing > 0) parts.push(plural(failing, "failing job", "failing jobs"));
  if (clusters > 0) parts.push(plural(clusters, "cluster", "clusters"));
  if (controllers > 0) parts.push(plural(controllers, "controller", "controllers"));
  if (count("updateCenter") > 0) parts.push("update center");
  return parts.join(" · ");
}

/** attentionCount is the count in the header. A cut server list adds a plus. */
export function attentionCount(items: AttentionItem[], cut: boolean): string {
  return `${items.length}${cut ? "+" : ""}`;
}

/** hhmm formats a time as local HH:MM. */
export function hhmm(iso: string): string {
  const d = new Date(iso);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function sameLocalDay(iso: string, now: number): boolean {
  const d = new Date(iso);
  const n = new Date(now);
  return d.getFullYear() === n.getFullYear() && d.getMonth() === n.getMonth() && d.getDate() === n.getDate();
}

/** jobTitle names a job by its tenant and its folder-relative job path. */
function jobTitle(run: { tenant: string; tenantFolder: string; jobPath?: string }): string {
  const job = stripTenantFolder(run.tenantFolder, run.jobPath ?? "") || run.jobPath || "";
  return `${run.tenant} / ${job}`;
}

export function inputTitle(w: PipelinesOverviewWaitingInput): string {
  const build = w.run.buildNumber !== undefined ? ` #${w.run.buildNumber}` : "";
  return `${jobTitle(w.run)}${build} waits for approval`;
}

/** inputDetail names the question, when the run started and who started it, and who may answer. */
export function inputDetail(w: PipelinesOverviewWaitingInput, now: number): string {
  const msg = w.input.message ? `"${w.input.message}"` : "This build waits for an answer.";
  const actor = runActorText(w.run);
  let text = `${msg} · started ${age(w.run.startedAt ?? w.run.createdAt, { now })}`;
  if (actor) text += ` by ${actor}`;
  if (w.input.submitter) text += ` · only ${w.input.submitter} may answer`;
  if (w.input.parameterCount > 0) text += ` · needs ${plural(w.input.parameterCount, "value", "values")}`;
  else if (w.inputCount > 1) text += ` · ${w.inputCount} inputs wait`;
  return text;
}

export function failingTitle(job: PipelinesOverviewFailingJob): string {
  return `${jobTitle(job.run)} is failing`;
}

/**
 * failingDetail names the failed build, its stage and reason, and the passed
 * run that came before the failures. It never names a log line.
 */
export function failingDetail(job: PipelinesOverviewFailingJob, now: number): string {
  const failedAt = job.run.endedAt ?? job.run.startedAt ?? job.run.createdAt;
  const when = sameLocalDay(failedAt, now) ? `at ${hhmm(failedAt)}` : age(failedAt, { now });
  const head = job.run.buildNumber !== undefined ? `#${job.run.buildNumber} failed` : "The run failed";
  let text = `${head} ${when}`;
  if (job.stage) text += ` in ${job.stage}`;
  if (job.testsFailed && job.testsFailed > 0) text += `: ${plural(job.testsFailed, "test", "tests")} failed`;
  else if (job.stageError) text += `: ${job.stageError}`;

  const k = job.failuresSincePass;
  const last = job.lastPassed;
  if (last) {
    const lastAt = last.endedAt ?? last.createdAt;
    const tag = last.buildNumber !== undefined ? `#${last.buildNumber} ` : "";
    if (k >= 2) text += ` · ${k} failures since ${tag}passed ${age(lastAt, { now })}`;
    else text += ` · last passed ${tag}${age(lastAt, { now })}`;
  } else if (k >= 2) {
    text += ` · ${k} failures, no passed run on record`;
  } else {
    text += " · no passed run on record";
  }
  return text;
}

export function clusterTitle(c: ClusterEntry): string {
  return `Cluster ${c.name} stopped reporting`;
}

export function clusterDetail(c: ClusterEntry, now: number): string {
  return `Last heartbeat ${age(c.lastHeartbeat, { variant: "heartbeat", now })}`;
}

export function updateCenterTitle(n: number): string {
  return `The update center cannot serve ${plural(n, "plugin", "plugins")}`;
}

/** controllerDetail names the attention message, or the label when the BFF sent none. */
export function controllerDetail(c: ControllerListItem): string {
  const a = c.attention;
  if (!a) return "";
  return a.message ?? ATTENTION_LABEL[a.kind] ?? a.kind;
}
