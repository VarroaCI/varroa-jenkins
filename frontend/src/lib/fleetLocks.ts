import type { FleetLock } from "../types";

/**
 * holdsLock answers true for the two states that name a holder run. A
 * waiting row names no holder, and a released row names none either.
 */
export function holdsLock(row: FleetLock): boolean {
  return row.state === "held" || row.state === "releasing";
}

/**
 * holderOf answers the row that holds the lock a waiting row waits for.
 *
 * The wire carries no holder field, so a page groups the list it already
 * holds by the lock name. It answers undefined when no row of that name
 * holds the lock: a holder on another cluster, or under another name
 * prefix and tenant, is absent from this list. A caller then states that
 * the lock is held elsewhere.
 *
 * The rule is one source for both lock tables, so the team-home card and
 * the operator page never disagree.
 */
export function holderOf(items: FleetLock[], row: FleetLock): FleetLock | undefined {
  if (row.state !== "waiting") return undefined;
  return items.find((other) => other.name === row.name && holdsLock(other));
}
