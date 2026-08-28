import type { UiSchema } from "@rjsf/utils";

/**
 * Curated `uiSchema` for the Tier-1 generated ControllerSpec form — the "Form"
 * tab of `SpecEditorCard`.
 *
 * Tier 1 is exactly the six fields not owned by a dedicated card, not edited
 * as YAML, and not dead-pending-CRD-removal: `resources`, `persistence`,
 * `rbacSpec`, `pluginSpec`, `backupSpec`, `className`. Everything else in the
 * ControllerSpec schema is excluded from rendering by `EXCLUDED_FROM_TIER1`
 * (see `excludedFields.ts`), which also excludes the dead `endpoint` field.
 *
 * The `ui:order` and `ui:title`s here replace RJSF's raw JSON property names
 * with a deliberate field order and human-readable labels. No `ui:readonly`
 * anywhere: no Tier-1 field is immutable — `persistence` is *ineffective until
 * teardown/recreate*, not immutable, so it carries `ui:help` instead.
 * `resources.limits`/`resources.requests` are free-form maps whose key inputs
 * get `ui:options.keySuggestions`, rendered as a non-restricting datalist.
 */
export const CONTROLLER_SPEC_UI_SCHEMA: UiSchema = {
  "ui:order": [
    "className",
    "resources",
    "persistence",
    "rbacSpec",
    "pluginSpec",
    "backupSpec",
  ],
  className: {
    "ui:title": "Controller class",
  },
  resources: {
    "ui:title": "Resources",
    requests: {
      "ui:title": "Requests",
      "ui:options": { keySuggestions: ["cpu", "memory", "ephemeral-storage"] },
    },
    limits: {
      "ui:title": "Limits",
      "ui:options": { keySuggestions: ["cpu", "memory", "ephemeral-storage"] },
    },
  },
  persistence: {
    "ui:title": "Persistence",
    "ui:help":
      "Persistence is applied at StatefulSet creation only — volumeClaimTemplates " +
      "are immutable, so edits to an existing controller take effect only after " +
      "teardown/recreate.",
    size: { "ui:title": "Size" },
    storageClass: { "ui:title": "Storage class" },
  },
  rbacSpec: {
    "ui:title": "RBAC",
    groups: { "ui:title": "Group-to-role bindings" },
  },
  pluginSpec: {
    "ui:title": "Plugins",
    entries: { "ui:title": "Plugin entries" },
    policy: { "ui:title": "Policy" },
  },
  backupSpec: {
    "ui:title": "Backups",
    enabled: { "ui:title": "Enabled" },
    schedule: { "ui:title": "Schedule" },
    retentionDays: { "ui:title": "Retention (days)" },
  },
};
