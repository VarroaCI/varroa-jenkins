import { useEffect, useState } from "react";

/** useNow answers the current time in milliseconds, refreshed every `ms` so relative text stays current. */
export function useNow(ms: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}
