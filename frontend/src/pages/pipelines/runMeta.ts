import type { TriggerSource } from "../../types";

/** QUEUE_WAIT_SHOWN_MS is the shortest queue wait the build page names.
 * A shorter wait is noise, so the page hides it. */
export const QUEUE_WAIT_SHOWN_MS = 5000;

/** MASKED_PARAM is the placeholder the plugin sends for a secret
 * parameter value. */
export const MASKED_PARAM = "<masked>";

/** TRIGGER_LABEL names each trigger source for a tooltip. */
export const TRIGGER_LABEL: Record<TriggerSource, string> = {
  user: "a user",
  webhook: "a webhook",
  cron: "a schedule",
  upstream: "an upstream build",
  replay: "a replay",
  scm: "an SCM poll or branch scan",
  other: "another cause",
};

/** queueWaitMs answers the time a run waited before it started. It answers
 * undefined when either time is missing or when the times do not order. */
export function queueWaitMs(run: { queuedAt?: string; startedAt?: string }): number | undefined {
  if (!run.queuedAt || !run.startedAt) return undefined;
  const wait = Date.parse(run.startedAt) - Date.parse(run.queuedAt);
  if (Number.isNaN(wait) || wait < 0) return undefined;
  return wait;
}

/** sortedParams lists the run parameters by name. */
export function sortedParams(params?: Record<string, string>): [string, string][] {
  if (!params) return [];
  return Object.entries(params).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}
