import { useNarrowViewport } from "./useNarrowViewport";

/** REDUCED_MOTION_QUERY matches a reader who asks for less motion. */
export const REDUCED_MOTION_QUERY = "(prefers-reduced-motion: reduce)";

/** useReducedMotion reports whether the reader asks for less motion. It is false where matchMedia is missing. */
export function useReducedMotion(): boolean {
  return useNarrowViewport(REDUCED_MOTION_QUERY);
}
