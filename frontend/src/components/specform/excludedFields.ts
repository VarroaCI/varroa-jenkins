// Field names owned by dedicated cards — excluded from Tier 1 generated form.
// Re-grep'd from ControllerDetail.tsx Configuration tab render (line ~1412):
//   <VersionCard .../>          → "version"
//   <HibernationCard .../>      → "powerState"
//   <ProbesCard .../>           → "probes"
//   <BundleCard .../>           → "composedBundleRef"
//   <PolicyEditForm .../>       → "reconciliationPolicy" (via card at line 1406)
//   <SpecEditorCard .../>       → "podOverrides" (Tier 2 YAML)
//                                → "resourceOverlay" (Tier 3 YAML)
//                                → "ingressSpec" (YAML tier — annotations is a
//                                  free-form key/value map the generated form
//                                  rendered unusably; see issue #429)
//                                → "miteSpec" (YAML tier — resources.requests/
//                                  limits are free-form maps the generated
//                                  form rendered unusably; see issue #429)
//
// rbacSpec, pluginSpec, backupSpec, resources, persistence, className have
// NO dedicated card — they render in Tier 1. "namespace" also renders in
// Tier 1 but is forced read-only there (immutable after creation; see #429).

export const EXCLUDED_FROM_TIER1: string[] = [
  "version",
  "powerState",
  "probes",
  "composedBundleRef",
  "reconciliationPolicy",
  "podOverrides",
  "resourceOverlay",
  "ingressSpec",
  "miteSpec",
];
