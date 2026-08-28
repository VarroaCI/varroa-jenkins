import { useQuery } from "@tanstack/react-query";
import { listCatalogSources, listCatalogItems, listComposedBundles } from "../api/client";

export function useCatalogSources(cluster: string | null, namespace?: string) {
  return useQuery({
    queryKey: ["catalog-sources", cluster, namespace],
    queryFn: () => listCatalogSources(cluster!, namespace),
    enabled: cluster !== null,
    refetchInterval: 30_000,
  });
}

export function useCatalogItems(cluster: string | null, params: { namespace?: string; source?: string; type?: string; q?: string }) {
  return useQuery({
    queryKey: ["catalog-items", cluster, params],
    queryFn: () => listCatalogItems(cluster!, params),
    enabled: cluster !== null,
    refetchInterval: 30_000,
  });
}

export function useComposedBundles(cluster: string | null, namespace?: string) {
  return useQuery({
    queryKey: ["composed-bundles", cluster, namespace],
    queryFn: () => listComposedBundles(cluster!, namespace),
    enabled: cluster !== null,
    refetchInterval: 30_000,
  });
}
