import { createContext, useContext } from "react";

/**
 * The current key set (and optional key suggestions) of the additionalProperties
 * map being rendered.
 *
 * `ObjectGroup` (the `ObjectFieldTemplate`) owns this: it receives the map's
 * whole `formData`, so it can derive every key in the map, and it receives the
 * map's own `uiSchema`, so it can read `ui:options.keySuggestions`. It wraps the
 * additional-property rows in `<MapKeysContext.Provider>`; each `MapEntry` row
 * consumes it for duplicate-key rejection and for the key-input `<datalist>`.
 *
 * A row rendered with no provider above it (a defensive edge case) gets `null`
 * from `useMapKeys()` and degrades to "no known siblings" rather than throwing.
 */
export interface MapKeysContextValue {
  /** Every current key in the map being rendered (from the map's `formData`). */
  keys: Set<string>;
  /** Optional key suggestions for the key input, from `ui:options.keySuggestions`.
   * Suggestions never restrict input — they only populate the datalist. */
  keySuggestions?: string[];
}

export const MapKeysContext = createContext<MapKeysContextValue | null>(null);

/** Read the map key context of the nearest enclosing map, or `null` when there
 * is no provider above the caller. */
export function useMapKeys(): MapKeysContextValue | null {
  return useContext(MapKeysContext);
}
