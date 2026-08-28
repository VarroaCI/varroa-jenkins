import { createContext, useContext } from "react";

/**
 * A reporter for in-progress map-key edits (renames).
 *
 * `MapEntry` commits a key rename on blur, so while the user is typing a new
 * key the only record of the edit is MapEntry's local input state — it never
 * changes the RJSF formData and never fires the form's `onChange`. `SpecEditorCard`
 * consumes this to treat the curated tier as dirty while a rename is in flight,
 * so a background refetch does not re-hydrate (and remount) the form and drop
 * the typed key and focus.
 *
 * The provider lives at `SpecEditorCard` (outside RJSF); `MapEntry` rows consume
 * it through the ordinary React tree. A row rendered with no provider above it
 * (defensive — e.g. `mapForm.test.tsx` rendering `MapEntry` directly) gets a
 * no-op reporter rather than throwing.
 */
export type KeyEditReporter = (editing: boolean) => void;

export const KeyEditContext = createContext<KeyEditReporter>(() => {});

/** Read the key-edit reporter of the nearest provider, or a no-op. */
export function useKeyEditReporter(): KeyEditReporter {
  return useContext(KeyEditContext);
}
