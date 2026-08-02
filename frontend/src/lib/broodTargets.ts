// broodTargetShape maps "cluster/ns/name" selection keys (as produced by
// BroodControllerPicker) onto the tenancy modes the brood API expects:
//   one cluster + one namespace   => {namespace, names:[bare], clusters:[c]}
//   one cluster + multi namespace => {names:[ns/name], clusters:[c]}
//   multi cluster                 => {names:[cluster/ns/name]} (no clusters)
//
// The explicit `namespace` is what keeps a single-namespace selection in the
// bare-name tenancy mode; without it the BFF defaults to the operator
// namespace, which requires ns/name-qualified names and rejects bare ones.
export function broodTargetShape(targets: string[]): {
  namespace?: string;
  names: string[];
  clusters?: string[];
} {
  const clusters = new Set(targets.map((s) => s.split("/")[0]));
  const namespaces = new Set(targets.map((s) => s.split("/")[1]));
  if (clusters.size === 1) {
    const c = clusters.values().next().value as string;
    if (namespaces.size === 1) {
      const ns = namespaces.values().next().value as string;
      return { namespace: ns, names: targets.map((s) => s.split("/")[2]), clusters: [c] };
    }
    return { names: targets.map((s) => s.split("/").slice(1).join("/")), clusters: [c] };
  }
  return { names: targets };
}
