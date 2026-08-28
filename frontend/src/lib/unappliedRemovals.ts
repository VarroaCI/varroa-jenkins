import type { UnappliedRemoval } from "../hooks/useControllers";

/**
 * A PATCH response may report requested spec removals (explicit JSON nulls)
 * that did not take effect: another field manager still owns the field, so
 * server-side apply retained it. Returns a NON-BLOCKING notice naming those
 * fields, or null when no removal was blocked so the caller can show its
 * ordinary success message. The save SUCCEEDED in either case — this is never
 * an error state.
 */
export function unappliedRemovalNotice(
  res: { unappliedRemovals?: UnappliedRemoval[] } | null | undefined,
): string | null {
  const unapplied = res?.unappliedRemovals ?? [];
  if (unapplied.length === 0) return null;
  const fields = unapplied.map((u) => u.field).join(", ");
  return `Saved, but ${fields} could not be removed (still owned by another manager)`;
}
