import { http, HttpResponse } from "msw";
import {
  createController,
  createComposedBundle,
  createComposedBundlePreview,
  createCatalogSource,
  createCatalogItem,
  createRole,
  createRoleBinding,
  createJenkinsRole,
  createJenkinsRoleBinding,
  createProvisioningConfig,
  createPermissions,
  createMeResponse,
  createAuthConfig,
  createControllerListItem,
  createControllerDetail,
} from "./factories";

const BASE = "/api/v1";

/**
 * Returns a default set of MSW handlers covering the critical API endpoints.
 * Pass overrides to customize responses for specific tests.
 */
export function createHandlers(overrides?: {
  meResponse?: ReturnType<typeof createMeResponse>;
  controllers?: ReturnType<typeof createControllerListItem>[];
  controller?: ReturnType<typeof createControllerDetail>;
  composedBundles?: ReturnType<typeof createComposedBundle>[];
  composedBundle?: ReturnType<typeof createComposedBundle>;
  preview?: ReturnType<typeof createComposedBundlePreview>;
  catalogSources?: ReturnType<typeof createCatalogSource>[];
  catalogItems?: ReturnType<typeof createCatalogItem>[];
  roles?: ReturnType<typeof createRole>[];
  roleBindings?: ReturnType<typeof createRoleBinding>[];
  jenkinsRoles?: ReturnType<typeof createJenkinsRole>[];
  jenkinsRoleBindings?: ReturnType<typeof createJenkinsRoleBinding>[];
  provisioningConfig?: ReturnType<typeof createProvisioningConfig>;
  permissions?: ReturnType<typeof createPermissions>;
  updateCenterStatus?: Record<string, unknown>;
  updateCenterPlugins?: Record<string, unknown>;
  fleetPluginsRollup?: Record<string, unknown>;
  fleetPluginsDrilldown?: Record<string, unknown>;
  fleetPluginsStatus?: number;
}) {
  const ctrls = overrides?.controllers ?? [createControllerListItem()];
  const bundles = overrides?.composedBundles ?? [createComposedBundle()];
  const bundle = overrides?.composedBundle ?? createComposedBundle();
  const preview = overrides?.preview ?? createComposedBundlePreview();
  const sources = overrides?.catalogSources ?? [createCatalogSource()];
  const items = overrides?.catalogItems ?? [createCatalogItem()];
  const roles = overrides?.roles ?? [createRole()];
  const roleBindings = overrides?.roleBindings ?? [createRoleBinding()];
  const jRoles = overrides?.jenkinsRoles ?? [createJenkinsRole()];
  const jRoleBindings = overrides?.jenkinsRoleBindings ?? [createJenkinsRoleBinding()];
  const prov = overrides?.provisioningConfig ?? createProvisioningConfig();
  const perms = overrides?.permissions ?? createPermissions();
  const me = overrides?.meResponse ?? createMeResponse();

  return [
    // Auth
    http.get(`${BASE}/auth-config`, () =>
      HttpResponse.json(createAuthConfig())),
    http.get(`${BASE}/me`, () =>
      HttpResponse.json(me)),
    http.get(`${BASE}/me/permissions`, () =>
      HttpResponse.json(perms)),
    http.post(`${BASE}/logout`, () =>
      new HttpResponse(null, { status: 204 })),

    // Controllers (core BFF aggregation, parameter-free)
    http.get(`${BASE}/controllers`, () =>
      HttpResponse.json({ apiVersion: "varroa.dev/v1alpha1", kind: "ControllerList", items: ctrls })),
    http.get(`${BASE}/clusters/:cluster/controllers/:namespace/:name`, ({ params }) => {
      const found = ctrls.find(
        (c) => c.name === params.name && c.namespace === params.namespace,
      );
      if (!found) return new HttpResponse("Not found", { status: 404 });
      return HttpResponse.json(createController({
        metadata: { name: params.name as string, namespace: params.namespace as string },
      }));
    }),
    http.post(`${BASE}/clusters/:cluster/controllers/:namespace`, () =>
      HttpResponse.json(createController(), { status: 201 })),
    http.patch(`${BASE}/clusters/:cluster/controllers/:namespace/:name`, () =>
      HttpResponse.json(createController())),
    http.delete(`${BASE}/clusters/:cluster/controllers/:namespace/:name`, () =>
      new HttpResponse(null, { status: 204 })),

    // Clusters
    http.get(`${BASE}/clusters`, () =>
      HttpResponse.json({ items: [{ name: "core", core: true, healthy: true, state: "active", lastHeartbeat: new Date().toISOString(), operatorVersion: "1.0", k8sVersion: "1.28", controllerCount: 5, connectedCount: 4 }] })),

    // Composed Bundles (cluster-scoped)
    http.get(`${BASE}/clusters/core/composedbundles`, () =>
      HttpResponse.json({ apiVersion: "varroa.dev/v1alpha1", kind: "ComposedBundleList", items: bundles })),
    http.get(`${BASE}/clusters/:cluster/composedbundles/:namespace/:name`, ({ params }) => {
      const found = bundles.find(
        (b) => b.metadata.name === params.name && b.metadata.namespace === params.namespace,
      );
      if (!found) return new HttpResponse("Not found", { status: 404 });
      return HttpResponse.json(found);
    }),
    http.post(`${BASE}/clusters/:cluster/composedbundles/validate`, () =>
      HttpResponse.json({ valid: true, errors: [], warnings: [] })),
    http.post(`${BASE}/clusters/:cluster/composedbundles/:namespace`, () =>
      HttpResponse.json(bundle, { status: 201 })),
    http.post(`${BASE}/clusters/:cluster/composedbundles/:namespace/preview`, () =>
      HttpResponse.json(preview)),
    http.put(`${BASE}/clusters/:cluster/composedbundles/:namespace/:name`, () =>
      HttpResponse.json(bundle)),
    http.delete(`${BASE}/clusters/:cluster/composedbundles/:namespace/:name`, () =>
      new HttpResponse(null, { status: 204 })),

    // Catalog Sources (cluster-scoped)
    http.get(`${BASE}/clusters/core/catalogsources`, () =>
      HttpResponse.json({ apiVersion: "varroa.dev/v1alpha1", kind: "CatalogSourceList", items: sources })),
    http.get(`${BASE}/clusters/:cluster/catalogsources/:namespace/:name`, ({ params }) => {
      const found = sources.find(
        (s) => s.metadata.name === params.name && s.metadata.namespace === params.namespace,
      );
      if (!found) return new HttpResponse("Not found", { status: 404 });
      return HttpResponse.json(found);
    }),
    http.post(`${BASE}/clusters/:cluster/catalogsources/:namespace`, () =>
      HttpResponse.json(createCatalogSource(), { status: 201 })),

    // Catalog Items (cluster-scoped)
    http.get(`${BASE}/clusters/core/catalogitems`, () =>
      HttpResponse.json({ apiVersion: "varroa.dev/v1alpha1", kind: "CatalogItemList", items })),
    http.get(`${BASE}/clusters/:cluster/catalogitems/:namespace/:name`, ({ params }) => {
      const found = items.find(
        (i) => i.metadata.name === params.name && i.metadata.namespace === params.namespace,
      );
      if (!found) return new HttpResponse("Not found", { status: 404 });
      return HttpResponse.json(found);
    }),

    // Roles (flat — VarroaRole, not cluster-scoped)
    http.get(`${BASE}/roles`, () =>
      HttpResponse.json({ items: roles })),
    http.get(`${BASE}/rolebindings`, () =>
      HttpResponse.json({ items: roleBindings })),

    // Jenkins Roles (cluster-scoped)
    http.get(`${BASE}/clusters/core/jenkinsroles`, () =>
      HttpResponse.json({ items: jRoles })),
    http.get(`${BASE}/clusters/core/jenkinsrolebindings`, () =>
      HttpResponse.json({ items: jRoleBindings })),

    // Provisioning Defaults (cluster-scoped)
    http.get(`${BASE}/clusters/core/provisioningdefaults/varroa-defaults`, () =>
      HttpResponse.json(prov)),
    http.put(`${BASE}/clusters/core/provisioningdefaults/varroa-defaults`, () =>
      HttpResponse.json(prov)),

    // Update Center
    http.get(`${BASE}/updatecenter`, () =>
      HttpResponse.json(overrides?.updateCenterStatus ?? { enabled: false, conditions: [], gaps: [], lastSyncTime: null, phase: "", pluginCount: 0, storeBytes: 0, storageType: "", pullThroughEnabled: false })),
    http.get(`${BASE}/updatecenter/plugins`, () =>
      HttpResponse.json(overrides?.updateCenterPlugins ?? { enabled: false, plugins: [] })),

    // Fleet plugin inventory
    http.get(`${BASE}/fleet/plugins`, () => {
      const status = overrides?.fleetPluginsStatus ?? 200;
      if (status >= 400) {
        return new HttpResponse(
          JSON.stringify({ error: "fleet plugin inventory is not available" }),
          { status, headers: { "Content-Type": "application/json" } },
        );
      }
      return HttpResponse.json(
        overrides?.fleetPluginsRollup ?? defaultFleetPluginsRollup(),
      );
    }),
    http.get(`${BASE}/fleet/plugins/:name`, () => {
      const status = overrides?.fleetPluginsStatus ?? 200;
      if (status >= 400) {
        return new HttpResponse(
          JSON.stringify({ error: "fleet plugin inventory is not available" }),
          { status, headers: { "Content-Type": "application/json" } },
        );
      }
      return HttpResponse.json(
        overrides?.fleetPluginsDrilldown ?? defaultFleetPluginDrilldown(),
      );
    }),
  ];
}

/** Default 502 fixture */
export function error502FleetPlugins(): Record<string, unknown> {
  return { error: "fleet plugin inventory is not available" };
}

/** Default "complete: false" rollup fixture */
export function incompleteFleetPluginsRollup(): Record<string, unknown> {
  return {
    items: [
      {
        name: "git-client",
        controllerCount: 1,
        versions: [{ version: "4.0.0", controllerCount: 1 }],
        classes: [{ class: "declared", controllerCount: 1 }],
      },
    ],
    coverage: {
      complete: false,
      controllersTotal: 1,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [],
      clustersNotCovered: 1,
    },
    clusters: [
      { name: "core", ok: true },
      { name: "remote", ok: false, error: "v1 covers the local cluster only (R22)" },
    ],
  };
}

/** Default "empty fleet" fixture */
function defaultFleetPluginsRollup(): Record<string, unknown> {
  return {
    items: [],
    coverage: {
      complete: true,
      controllersTotal: 0,
      controllersReporting: 0,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [],
      clustersNotCovered: 0,
    },
    clusters: [{ name: "core", ok: true }],
  };
}

/** Default drilldown fixture */
function defaultFleetPluginDrilldown(): Record<string, unknown> {
  return {
    name: "git-client",
    items: [],
    versions: [],
    coverage: {
      complete: true,
      controllersTotal: 1,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [],
      clustersNotCovered: 0,
    },
    clusters: [{ name: "core", ok: true }],
  };
}

/** Rollup with controllersMissing */
export function rollupWithMissingControllers(): Record<string, unknown> {
  return {
    items: [],
    coverage: {
      complete: true,
      controllersTotal: 3,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [
        { cluster: "core", namespace: "ns", name: "ctrl-b", reason: "never-reported" },
        { cluster: "core", namespace: "ns", name: "ctrl-c", reason: "hibernated" },
      ],
      clustersNotCovered: 0,
    },
    clusters: [{ name: "core", ok: true }],
  };
}

/** Rollup with controllersDetailStale > 0 */
export function rollupWithDetailStale(): Record<string, unknown> {
  return {
    items: [
      {
        name: "git-client",
        controllerCount: 1,
        versions: [{ version: "4.0.0", controllerCount: 1 }],
        classes: [{ class: "declared", controllerCount: 1 }],
      },
    ],
    coverage: {
      complete: false,
      controllersTotal: 1,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 1,
      controllersMissing: [],
      clustersNotCovered: 1,
    },
    clusters: [
      { name: "core", ok: true },
      { name: "remote", ok: false, error: "v1 covers the local cluster only (R22)" },
    ],
  };
}

/** Rollup with an unknown class label */
export function rollupWithUnknownClass(): Record<string, unknown> {
  return {
    items: [
      {
        name: "mystery-plugin",
        controllerCount: 1,
        versions: [{ version: "99.0", controllerCount: 1 }],
        classes: [{ class: "future-label-xyz", controllerCount: 1 }],
      },
    ],
    coverage: {
      complete: true,
      controllersTotal: 1,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [],
      clustersNotCovered: 0,
    },
    clusters: [{ name: "core", ok: true }],
  };
}

/** Drilldown with an unknown class label */
export function drilldownWithUnknownClass(): Record<string, unknown> {
  return {
    name: "mystery-plugin",
    items: [
      {
        cluster: "core",
        namespace: "ns",
        controller: "ctrl-a",
        version: "99.0",
        class: "future-label-xyz",
        source: "jenkins-api",
        collectedAt: "2025-01-15T10:00:00Z",
        detailPath: "/api/v1/clusters/core/controllers/ns/ctrl-a/plugins",
        detailStale: false,
        stale: false,
        degraded: false,
        truncated: false,
        optionalEdgesDropped: false,
        bootstrapApproximate: false,
      },
    ],
    versions: [{ version: "99.0", controllerCount: 1 }],
    coverage: {
      complete: true,
      controllersTotal: 1,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [],
      clustersNotCovered: 0,
    },
    clusters: [{ name: "core", ok: true }],
  };
}

/** Drilldown with bootstrapApproximate independent of degraded */
export function drilldownWithBootstrapApproximate(): Record<string, unknown> {
  return {
    name: "git-client",
    items: [
      {
        cluster: "core",
        namespace: "ns",
        controller: "ctrl-a",
        version: "4.0.0",
        class: "bootstrap",
        source: "jenkins-api",
        collectedAt: "2025-01-15T10:00:00Z",
        detailPath: "/api/v1/clusters/core/controllers/ns/ctrl-a/plugins",
        detailStale: false,
        stale: false,
        degraded: false,
        truncated: false,
        optionalEdgesDropped: false,
        bootstrapApproximate: true,
      },
    ],
    versions: [{ version: "4.0.0", controllerCount: 1 }],
    coverage: {
      complete: true,
      controllersTotal: 1,
      controllersReporting: 1,
      controllersStale: 0,
      controllersDegraded: 0,
      controllersTruncated: 0,
      controllersDetailStale: 0,
      controllersMissing: [],
      clustersNotCovered: 0,
    },
    clusters: [{ name: "core", ok: true }],
  };
}
