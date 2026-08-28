import { useEffect } from "react";
import { useSearchParams } from "react-router-dom";
import type { ClusterEntry } from "../types";
import { coreOf, useClusters } from "./useClusters";

export interface ConfigurationCluster {
  cluster: string | null;
  entry: ClusterEntry | null;
  ready: boolean;
}

export function useConfigurationCluster(): ConfigurationCluster {
  const [searchParams, setSearchParams] = useSearchParams();
  const { data, isLoading } = useClusters();
  const ready = !isLoading;
  const eligible = ready ? (data ?? []).filter((entry) => entry.healthy && entry.state === "active") : [];
  const requested = searchParams.get("cluster");
  const entry = eligible.find((candidate) => candidate.name === requested) ?? coreOf(eligible) ?? eligible[0] ?? null;

  useEffect(() => {
    if (!ready || !entry || requested === entry.name) return;
    const next = new URLSearchParams(searchParams);
    next.set("cluster", entry.name);
    setSearchParams(next, { replace: true });
  }, [entry, ready, requested, searchParams, setSearchParams]);

  return { cluster: entry?.name ?? null, entry, ready };
}
