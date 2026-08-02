/**
 * Parses a `<input type="number">` onChange value into a valid integer, or
 * the empty-string sentinel when the field is blank.
 *
 * Returning `""` (instead of immediately falling back to a default) lets the
 * field stay empty while the user is mid-edit. Clamping back to a default
 * belongs in an onBlur handler, never in onChange: parsing straight to a
 * default inside onChange means backspacing to clear the field snaps the
 * displayed value right back, so the next digit typed appends onto the
 * default instead of replacing it (issue #428).
 */
export function parseClearableInt(raw: string): number | "" {
  if (raw.trim() === "") return "";
  const n = parseInt(raw, 10);
  return Number.isNaN(n) ? "" : n;
}

/** Clamps a clearable numeric field to `min`, falling back to `fallback` (or `min`) when empty or below it. Use in onBlur. */
export function clampClearableInt(value: number | "", min: number, fallback: number = min): number {
  if (value === "" || value < min) return fallback;
  return value;
}
