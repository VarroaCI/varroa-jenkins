// Field names excluded from the Tier-1 generated form (the "Form" tab of
// SpecEditorCard), in three categories:
//
//   1. Owned by dedicated cards, read from ControllerDetail.tsx's
//      Configuration tab render:
//        <VersionCard .../>      → "version"
//        <HibernationCard .../>  → "hibernation" (writes spec.hibernation)
//        <ProbesCard .../>       → "probes"
//        <BundleCard .../>       → "composedBundleRef"
//        <PolicyEditForm .../>   → "reconciliationPolicy" (via card at line 1406)
//        <SpecEditorCard .../>   → "podOverrides" (Tier 2 YAML)
//                                 → "resourceOverlay" (Tier 3 YAML)
//
//   2. Edited as YAML — podOverrides and resourceOverlay render in
//      SpecEditorCard's YAML tiers instead of the generated form.
//
//   3. Dead, pending CRD removal — "endpoint" has zero Go readers or writers,
//      so it used to render an editable control that did nothing. Excluding it
//      hides that control; `stripExcluded` (SpecEditorCard.tsx) keeps it out of
//      both the hydration snapshot and the curated draft, so an existing
//      endpoint value is preserved, never deleted. Removing the CRD field
//      itself stays out of scope.
//
// The fields that remain — resources, persistence, rbacSpec, pluginSpec,
// backupSpec, className, cell, headless, ingressSpec, miteSpec — render in
// Tier 1 (controllerUiSchema.ts carries their labels and order). resources,
// ingressSpec.annotations, and miteSpec.resources are map editors with key
// suggestions and free-form keys.

export const EXCLUDED_FROM_TIER1: string[] = [
  "version",
  "hibernation",
  "powerState",
  "probes",
  "composedBundleRef",
  "reconciliationPolicy",
  "podOverrides",
  "resourceOverlay",
  "endpoint",
];
