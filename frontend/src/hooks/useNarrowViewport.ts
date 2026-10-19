import { useEffect, useState } from "react";

/** NARROW_QUERY is the shell's single phone breakpoint. */
export const NARROW_QUERY = "(max-width: 639.98px)";

/** useNarrowViewport reports whether the viewport is below 640px. It is false where matchMedia is missing. */
export function useNarrowViewport(query: string = NARROW_QUERY): boolean {
  const [narrow, setNarrow] = useState<boolean>(() => matches(query));
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mql = window.matchMedia(query);
    const onChange = () => setNarrow(mql.matches);
    onChange();
    mql.addEventListener?.("change", onChange);
    return () => mql.removeEventListener?.("change", onChange);
  }, [query]);
  return narrow;
}

function matches(query: string): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia(query).matches;
}
