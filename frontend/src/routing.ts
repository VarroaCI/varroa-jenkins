export function clusterQuery(cluster: string | null | undefined): string {
  return cluster ? `?cluster=${encodeURIComponent(cluster)}` : "";
}

export function withCluster(path: string, cluster: string): string {
  const [pathname, query = ""] = path.split("?", 2);
  const params = new URLSearchParams(query);
  params.set("cluster", cluster);
  return `${pathname}?${params.toString()}`;
}

export function configurationLink(path: string, cluster: string): string {
  return withCluster(path, cluster);
}

export function controllerRoute(cluster: string, namespace: string, name: string): string {
  return `/controllers/${encodeURIComponent(cluster)}/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
}
