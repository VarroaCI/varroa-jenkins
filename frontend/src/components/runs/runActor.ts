/**
 * displayActor answers the actor text to show, or undefined when no actor is
 * known. The server reports "unknown" for a run with no recorded actor.
 * It reports "timer" for a cron run, which reads as "schedule".
 * This file has no imports, so no import cycle can start here.
 */
export function displayActor(actor?: string): string | undefined {
  if (!actor || actor === "unknown") return undefined;
  if (actor === "timer") return "schedule";
  return actor;
}

/**
 * isMachineActor is true for an actor that names a trigger, not a person: a
 * cron run, an SCM run, or an upstream run. A fleet row omits such an actor,
 * because its trigger label already names the cause.
 */
export function isMachineActor(actor?: string): boolean {
  return actor === "timer" || actor === "scm" || (actor?.startsWith("upstream:") ?? false);
}
