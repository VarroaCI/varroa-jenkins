// Field names excluded from the Tier-1 generated form (the "Form" tab of
// SpecEditorCard), in three categories:
//
//   1. Owned by dedicated cards, re-grep'd from ControllerDetail.tsx's
//      Configuration tab render (line ~1412):
//        <VersionCard .../>      → "version"
//        <HibernationCard .../>  → "hibernation" (writes spec.hibernation)
//        <ProbesCard .../>       → "probes"
//        <BundleCard .../>       → "composedBundleRef"
//        <PolicyEditForm .../>   → "reconciliationPolicy" (via card at line 1406)
//        <SpecEditorCard .../>   → "podOverrides" (Tier 2 YAML)
//                                 → "resourceOverlay" (Tier 3 YAML)
//                                 → "ingressSpec" (YAML tier)
//                                 → "miteSpec" (YAML tier)
//
//   2. Edited as YAML — podOverrides, resourceOverlay, ingressSpec and
//      miteSpec render in SpecEditorCard's YAML tiers instead of the generated
//      form. ingressSpec.annotations and miteSpec.resources.requests/limits
//      are free-form key/value maps the generated form rendered unusably
//      (issue #429), which is why those two moved to YAML.
//
//   3. Dead, pending CRD removal — "endpoint" has zero Go readers or writers,
//      so it used to render an editable control that did nothing. Excluding it
//      hides that control; `stripExcluded` (SpecEditorCard.tsx) keeps it out of
//      both the hydration snapshot and the curated draft, so an existing
//      endpoint value is preserved, never deleted. Removing the CRD field
//      itself stays out of scope.
//
// The six fields that remain — resources, persistence, rbacSpec, pluginSpec,
// backupSpec, className — render in Tier 1 (controllerUiSchema.ts carries
// their labels and order). resources is a real Tier-1 field now: the map
// editor renders editable limits/requests rows with key suggestions.

export const EXCLUDED_FROM_TIER1: string[] = [
  "version",
  "hibernation",
  "powerState",
  "probes",
  "composedBundleRef",
  "reconciliationPolicy",
  "podOverrides",
  "resourceOverlay",
  "ingressSpec",
  "miteSpec",
  "endpoint",
];
