/**
 * displayActor answers the actor text to show, or undefined when no actor is
 * known. The server reports "unknown" for a run with no recorded actor.
 */
export function displayActor(actor?: string): string | undefined {
  if (!actor || actor === "unknown") return undefined;
  return actor;
}
