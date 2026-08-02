import { useState, useCallback } from "react";
import { getControllerDiff } from "../api/client";
import type { ControllerDiff } from "../types";

export function useControllerDiff(cluster: string, ns: string, name: string) {
  const [diff, setDiff] = useState<ControllerDiff | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchDiff = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await getControllerDiff(cluster, ns, name);
      setDiff(result);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to fetch diff");
    } finally {
      setLoading(false);
    }
  }, [cluster, ns, name]);

  return { diff, loading, error, fetchDiff };
}
