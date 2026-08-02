import { useQuery } from "@tanstack/react-query";
import { listFleetPlugins, getFleetPlugin } from "../api/client";
import type { FleetPluginsParams } from "../api/client";

export function useFleetPluginsRollup(params?: FleetPluginsParams) {
  return useQuery({
    queryKey: ["fleet-plugins", params],
    queryFn: () => listFleetPlugins(params),
    refetchInterval: 30_000,
  });
}

export function useFleetPluginDrilldown(name: string | null, params?: FleetPluginsParams) {
  return useQuery({
    queryKey: ["fleet-plugins", name, params],
    queryFn: () => getFleetPlugin(name!, params),
    enabled: name !== null,
    refetchInterval: 30_000,
  });
}
