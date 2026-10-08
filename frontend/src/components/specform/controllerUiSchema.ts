import type { UiSchema } from "@rjsf/utils";

/**
 * Curated `uiSchema` for the Tier-1 generated ControllerSpec form — the "Form"
 * tab of `SpecEditorCard`.
 *
 * Tier 1 is every ControllerSpec field not owned by a dedicated card, not
 * edited as YAML, and not dead-pending-CRD-removal: `className`, `resources`,
 * `persistence`, `rbacSpec`, `pluginSpec`, `backupSpec`, `cell`, `headless`,
 * `ingressSpec`, `miteSpec`. Everything else in the ControllerSpec schema is
 * excluded from rendering by `EXCLUDED_FROM_TIER1` (see `excludedFields.ts`),
 * which also excludes the dead `endpoint` field.
 *
 * The `ui:order` and `ui:title`s here replace RJSF's raw JSON property names
 * with a deliberate field order and human-readable labels. `ingressSpec.mode`
 * is `ui:readonly` because the CRD and the operator refuse a change after
 * create. `persistence` is *ineffective until teardown/recreate*, not
 * immutable, so it carries `ui:help` instead. Map fields (`resources.*`,
 * `ingressSpec.annotations`, `miteSpec.resources.*`) are free-form maps whose
 * key inputs get `ui:options.keySuggestions`, rendered as a non-restricting
 * datalist.
 */
export const CONTROLLER_SPEC_UI_SCHEMA: UiSchema = {
  "ui:order": [
    "className",
    "resources",
    "persistence",
    "rbacSpec",
    "pluginSpec",
    "backupSpec",
    "cell",
    "headless",
    "ingressSpec",
    "miteSpec",
    "*",
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
  cell: {
    "ui:title": "Cell",
    "ui:help":
      "A cell hosts tenant pipelines for several Teams. cell.enabled cannot " +
      "change after creation; maxTenants bounds placement.",
    enabled: { "ui:title": "Enabled" },
    maxTenants: { "ui:title": "Max tenants" },
  },
  backupSpec: {
    "ui:title": "Backups",
    enabled: { "ui:title": "Enabled" },
    schedule: { "ui:title": "Schedule" },
    retentionDays: { "ui:title": "Retention (days)" },
  },
  headless: {
    "ui:title": "Headless",
    "ui:help":
      "Blocks the Jenkins web UI for every non-admin caller. A cell is " +
      "always headless, so this switch matters only for a one-tenant controller.",
  },
  ingressSpec: {
    "ui:title": "Ingress",
    host: { "ui:title": "Host" },
    tlsSecretName: { "ui:title": "TLS secret" },
    ingressClassName: { "ui:title": "Ingress class" },
    mode: {
      "ui:title": "Routing mode",
      "ui:readonly": true,
      "ui:help": "The routing mode cannot change after the controller is created.",
    },
    annotations: {
      "ui:title": "Annotations",
      "ui:options": {
        keySuggestions: [
          "nginx.ingress.kubernetes.io/proxy-body-size",
          "cert-manager.io/cluster-issuer",
        ],
      },
    },
  },
  miteSpec: {
    "ui:title": "Mite sidecar",
    image: { "ui:title": "Mite image" },
    imagePullPolicy: { "ui:title": "Mite image pull policy" },
    resources: {
      "ui:title": "Mite resources",
      requests: {
        "ui:title": "Mite requests",
        "ui:options": { keySuggestions: ["cpu", "memory"] },
      },
      limits: {
        "ui:title": "Mite limits",
        "ui:options": { keySuggestions: ["cpu", "memory"] },
      },
    },
  },
};
