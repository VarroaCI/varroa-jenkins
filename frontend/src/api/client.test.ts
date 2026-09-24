import { describe, it, expect, vi, beforeEach } from "vitest";

// Mock the underlying fetch helper so we can assert how the client builds requests.
const mockBffFetch = vi.fn();
const mockBffFetchText = vi.fn();

const mockGetToken = vi.fn<() => string | null>(() => null);

vi.mock("../hooks/useApi", async () => {
  const mod = await vi.importActual<typeof import("../hooks/useApi")>("../hooks/useApi");
  return {
    bffFetch: (...args: unknown[]) => mockBffFetch(...args),
    bffFetchText: (...args: unknown[]) => mockBffFetchText(...args),
    getToken: () => mockGetToken(),
    ApiError: mod.ApiError,
  };
});

import {
  listControllers,
  getController,
  createController,
  deleteController,
  updateController,
  approveRestart,
  reprovisionController,
  restartController,
  setPowerState,
  hibernateController,
  wakeController,
  updateIngress,
  preflightController,
  renderController,
  controllerEventsUrl,
  listRoles,
  getRole,
  createRole,
  updateRole,
  deleteRole,
  listRoleBindings,
  getRoleBinding,
  createRoleBinding,
  updateRoleBinding,
  deleteRoleBinding,
  getMyPermissions,
  listJenkinsRoles,
  getJenkinsRole,
  createJenkinsRole,
  updateJenkinsRole,
  deleteJenkinsRole,
  listJenkinsRoleBindings,
  getJenkinsRoleBinding,
  createJenkinsRoleBinding,
  updateJenkinsRoleBinding,
  deleteJenkinsRoleBinding,
  listCatalogSources,
  getCatalogSource,
  createCatalogSource,
  updateCatalogSource,
  deleteCatalogSource,
  syncCatalogSource,
  listCatalogItems,
  getCatalogItem,
  listComposedBundles,
  getComposedBundle,
  previewComposedBundle,
  createComposedBundle,
  updateComposedBundle,
  deleteComposedBundle,
  validateComposedBundle,
  getProvisioningConfig,
  getProvisioningDefaults,
  updateProvisioningDefaults,
  getVersionProfiles,
  createVersionProfile,
  updateVersionProfile,
  deleteVersionProfile,
  createApiKey,
  listApiKeys,
  revokeApiKey,
  rotateApiKey,
  adminListUserApiKeys,
  adminRevokeApiKey,
  parsePreflightChecks,
  listBroodOperations,
  getBroodOperation,
  createBroodOperation,
  deleteBroodOperation,
  suspendBroodOperation,
  previewBroodOperation,
  broodStreamUrl,
  listClusters,
  listPipelines,
  getPipelineJob,
  listRuns,
  createRun,
  getRun,
  runLogUrl,
  listRunArtifacts,
  downloadRunArtifact,
  listRunTests,
  getTestCaseHistory,
  listRunChanges,
  cancelRun,
  rerunRun,
  listTeams,
  getTeam,
  createTeam,
  updateTeam,
  deleteTeam,
  listRunners,
} from "./client";

beforeEach(() => {
  mockBffFetch.mockReset();
  mockBffFetchText.mockReset();
  mockGetToken.mockReset();
  mockGetToken.mockReturnValue(null);
});

// ---- Controllers ----

describe("Controller API", () => {
  it("listControllers GETs /controllers", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listControllers();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/controllers");
  });

  it("getController uses the ctrlBase path", async () => {
    mockBffFetch.mockResolvedValue({});
    await getController("core", "ctrl-1", "team-a");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1");
  });

  it("createController POSTs a JSON body", async () => {
    mockBffFetch.mockResolvedValue({});
    await createController("core", "team-a", { metadata: { name: "ctrl-1" } });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a");
    expect(opts.method).toBe("POST");
  });

  it("createController omits empty probes and preserves populated probes", async () => {
    mockBffFetch.mockResolvedValue({});
    await createController("core", "team-a", {
      metadata: { name: "ctrl-1" },
      spec: {
        probes: {
          startup: {},
          liveness: { disabled: true },
        },
      },
    });
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({
      apiVersion: "varroa.dev/v1alpha1",
      kind: "Controller",
      metadata: { name: "ctrl-1" },
      spec: {
        probes: { liveness: { disabled: true } },
      },
    });
  });

  it("createController preserves fully populated probes", async () => {
    mockBffFetch.mockResolvedValue({});
    await createController("core", "team-a", {
      metadata: { name: "ctrl-1" },
      spec: {
        probes: {
          startup: {
            initialDelaySeconds: 21,
            periodSeconds: 22,
            timeoutSeconds: 23,
            failureThreshold: 24,
            successThreshold: 25,
          },
          readiness: {
            initialDelaySeconds: 1,
            periodSeconds: 2,
            timeoutSeconds: 3,
            failureThreshold: 4,
            successThreshold: 1,
          },
          liveness: {
            initialDelaySeconds: 5,
            periodSeconds: 6,
            timeoutSeconds: 7,
            failureThreshold: 8,
            successThreshold: 1,
          },
        },
      },
    });
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({
      apiVersion: "varroa.dev/v1alpha1",
      kind: "Controller",
      metadata: { name: "ctrl-1" },
      spec: {
        probes: {
          startup: {
            initialDelaySeconds: 21,
            periodSeconds: 22,
            timeoutSeconds: 23,
            failureThreshold: 24,
            successThreshold: 25,
          },
          readiness: {
            initialDelaySeconds: 1,
            periodSeconds: 2,
            timeoutSeconds: 3,
            failureThreshold: 4,
            successThreshold: 1,
          },
          liveness: {
            initialDelaySeconds: 5,
            periodSeconds: 6,
            timeoutSeconds: 7,
            failureThreshold: 8,
            successThreshold: 1,
          },
        },
      },
    });
  });

  it("deleteController DELETEs the controller", async () => {
    mockBffFetch.mockResolvedValue({});
    await deleteController("core", "ctrl-1", "team-a");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1");
    expect(opts.method).toBe("DELETE");
  });

  it("updateController PATCHes the body", async () => {
    mockBffFetch.mockResolvedValue({});
    const patch = { spec: { version: "3.0" } };
    await updateController("core", "ctrl-1", "team-a", patch);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1");
    expect(opts.method).toBe("PATCH");
    expect(JSON.parse(opts.body)).toEqual(patch);
  });

  it("updateController omits empty probes from merge patches", async () => {
    mockBffFetch.mockResolvedValue({});
    const patch = { spec: { probes: { startup: {}, readiness: { disabled: true } } } };
    await updateController("core", "ctrl-1", "team-a", patch);
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ spec: { probes: { readiness: { disabled: true } } } });
  });

  it("updateController omits probes entirely when all probe entries are empty", async () => {
    mockBffFetch.mockResolvedValue({});
    const patch = { spec: { probes: { startup: {}, readiness: {}, liveness: {} } } };
    await updateController("core", "ctrl-1", "team-a", patch);
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ spec: {} });
  });

  it("approveRestart POSTs the restart approval", async () => {
    mockBffFetch.mockResolvedValue({});
    await approveRestart("core", "team-a", "ctrl-1", "restart");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1/approve");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ action: "restart" });
  });

  it("reprovisionController POSTs /reprovision", async () => {
    mockBffFetch.mockResolvedValue({});
    await reprovisionController("core", "ctrl-1", "team-a");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1/reprovision");
    expect(opts.method).toBe("POST");
  });

  it("restartController POSTs /restart", async () => {
    mockBffFetch.mockResolvedValue({});
    await restartController("core", "ctrl-1", "team-a");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1/restart");
    expect(opts.method).toBe("POST");
  });

  it("setPowerState calls updateController with the spec", async () => {
    mockBffFetch.mockResolvedValue({});
    await setPowerState("core", "ctrl-1", "team-a", "Stopped");
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ spec: { powerState: "Stopped" } });
  });

  it("hibernateController POSTs /hibernate", async () => {
    mockBffFetch.mockResolvedValue({ status: "triggered" });
    await hibernateController("core", "ctrl-1", "team-a");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1/hibernate");
    expect(opts.method).toBe("POST");
  });

  it("wakeController POSTs /wake", async () => {
    mockBffFetch.mockResolvedValue({ status: "triggered" });
    await wakeController("core", "ctrl-1", "team-a");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/ctrl-1/wake");
    expect(opts.method).toBe("POST");
  });

  it("updateIngress calls updateController with the ingress spec", async () => {
    mockBffFetch.mockResolvedValue({});
    const spec = { mode: "subdomain" };
    await updateIngress("core", "ctrl-1", "team-a", spec);
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ spec: { ingressSpec: spec } });
  });

  describe("updateController force + conflict", () => {
    async function apiError(status: number, body?: Record<string, unknown>) {
      const { ApiError } = await import("../hooks/useApi");
      return new ApiError(status, `Error ${status}`, body);
    }

    it("appends ?force=true when opts.force is set", async () => {
      mockBffFetch.mockResolvedValue({});
      await updateController("core", "ctrl-1", "team-a", { spec: { version: "3.0" } }, { force: true });
      const [url] = mockBffFetch.mock.calls[0];
      expect(url).toContain("?force=true");
    });

    it("does not append force param when opts is not passed", async () => {
      mockBffFetch.mockResolvedValue({});
      await updateController("core", "ctrl-1", "team-a", { spec: { version: "3.0" } });
      const [url] = mockBffFetch.mock.calls[0];
      expect(url).not.toContain("?force");
    });

    it("throws ControllerConflictError on a 409 with conflicts array", async () => {
      const err = await apiError(409, {
        error: "field conflict",
        conflicts: [{ field: ".spec.version", manager: "other-manager", message: "conflict" }],
      });
      mockBffFetch.mockRejectedValue(err);

      const { ControllerConflictError } = await import("./client");
      await expect(updateController("core", "ctrl-1", "team-a", { spec: { version: "3.0" } }))
        .rejects.toThrow(ControllerConflictError);
    });

    it("ControllerConflictError carries the conflicts array", async () => {
      const err = await apiError(409, {
        error: "field conflict",
        conflicts: [{ field: ".spec.version", manager: "other-manager", message: "conflict" }],
      });
      mockBffFetch.mockRejectedValue(err);

      try {
        await updateController("core", "ctrl-1", "team-a", { spec: { version: "3.0" } });
      } catch (e: unknown) {
        const ce = e as import("./client").ControllerConflictError;
        expect(ce.conflicts).toHaveLength(1);
        expect(ce.conflicts[0].field).toBe(".spec.version");
        expect(ce.conflicts[0].manager).toBe("other-manager");
      }
    });

    it("throws ControllerConflictError even when conflicts array is missing", async () => {
      const err = await apiError(409, { error: "conflict" });
      mockBffFetch.mockRejectedValue(err);

      try {
        await updateController("core", "ctrl-1", "team-a", { spec: { version: "3.0" } });
      } catch (e: unknown) {
        const { ControllerConflictError } = await import("./client");
        expect(e).toBeInstanceOf(ControllerConflictError);
        const ce = e as import("./client").ControllerConflictError;
        expect(ce.conflicts).toEqual([]);
      }
    });

    it("re-throws non-409 errors as-is", async () => {
      const err = await apiError(400, { error: "invalid" });
      mockBffFetch.mockRejectedValue(err);

      await expect(updateController("core", "ctrl-1", "team-a", { spec: { version: "3.0" } }))
        .rejects.toThrow(); // ApiError — we just need it to throw, not change the error type
    });
  });

  it("preflightController POSTs /preflight", async () => {
    mockBffFetch.mockResolvedValue({ checks: [] });
    const checks = await preflightController("core", "team-a", { controller: { metadata: { name: "ctrl-1" } } });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/controllers/team-a/preflight");
    expect(opts.method).toBe("POST");
    expect(checks).toEqual({ checks: [] });
  });

  it("renderController POSTs /render", async () => {
    mockBffFetchText.mockResolvedValue("---");
    const result = await renderController("core", "team-a", { controller: { metadata: { name: "ctrl-1" } } as Record<string, unknown> });
    expect(result).toBe("---");
  });

  it("controllerEventsUrl builds the SSE path", () => {
    const url = controllerEventsUrl("core", "team-a", "ctrl-1");
    expect(url).toContain("/clusters/core/controllers/team-a/ctrl-1/events");
  });
});

// ---- Clusters ----

describe("Cluster API", () => {
  it("listClusters GETs /clusters", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listClusters();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters");
  });

// Cluster/namespace tests above. Removed deployable-namespaces test (function not exported).
});

// ---- JenkinsRole CRUD ----

describe("JenkinsRole API", () => {
  it("listJenkinsRoles GETs /clusters/core/jenkinsroles", async () => {
    await listJenkinsRoles("core");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsroles");
  });

  it("getJenkinsRole encodes the name", async () => {
    await getJenkinsRole("core", "jr with spaces");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsroles/jr%20with%20spaces");
  });

  it("createJenkinsRole POSTs the role", async () => {
    const role = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "JenkinsRole" as const, metadata: { name: "jr" }, spec: { roleType: "Global" as const, permissions: [] } };
    await createJenkinsRole("core", role);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsroles");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(role);
  });

  it("updateJenkinsRole PUTs the role", async () => {
    const role = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "JenkinsRole" as const, metadata: { name: "jr" }, spec: { roleType: "Global" as const, permissions: [] } };
    await updateJenkinsRole("core", "jr", role);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsroles/jr");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual(role);
  });

  it("deleteJenkinsRole sends DELETE", async () => {
    await deleteJenkinsRole("core", "jr");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsroles/jr");
    expect(opts.method).toBe("DELETE");
  });
});

// ---- JenkinsRoleBinding CRUD ----

describe("JenkinsRoleBinding API", () => {
  it("listJenkinsRoleBindings GETs /clusters/core/jenkinsrolebindings", async () => {
    await listJenkinsRoleBindings("core");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsrolebindings");
  });

  it("getJenkinsRoleBinding encodes the name", async () => {
    await getJenkinsRoleBinding("core", "jrb name");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsrolebindings/jrb%20name");
  });

  it("createJenkinsRoleBinding POSTs the binding", async () => {
    const binding = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "JenkinsRoleBinding" as const, metadata: { name: "jrb" }, spec: { subjects: [], roleRef: "jr" } };
    await createJenkinsRoleBinding("core", binding);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsrolebindings");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(binding);
  });

  it("updateJenkinsRoleBinding PUTs the binding", async () => {
    const binding = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "JenkinsRoleBinding" as const, metadata: { name: "jrb" }, spec: { subjects: [], roleRef: "jr" } };
    await updateJenkinsRoleBinding("core", "jrb", binding);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsrolebindings/jrb");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual(binding);
  });

  it("deleteJenkinsRoleBinding sends DELETE", async () => {
    await deleteJenkinsRoleBinding("core", "jrb");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/jenkinsrolebindings/jrb");
    expect(opts.method).toBe("DELETE");
  });
});

// ---- Catalog Sources ----

describe("Catalog Sources API", () => {
  it("listCatalogSources without namespace", async () => {
    await listCatalogSources("core");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources");
  });

  it("listCatalogSources with namespace query param", async () => {
    await listCatalogSources("core", "my-ns");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources?namespace=my-ns");
  });

  it("getCatalogSource fetches by ns and name", async () => {
    await getCatalogSource("core", "src", "my-ns");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources/my-ns/src");
  });

  it("createCatalogSource POSTs to namespaced path", async () => {
    await createCatalogSource("core", "my-ns", { apiVersion: "varroa.dev/v1alpha1" as const, kind: "CatalogSource" as const, metadata: { name: "src" }, spec: { repoURL: "https://example.com" } });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources/my-ns");
    expect(opts.method).toBe("POST");
  });

  it("updateCatalogSource PUTs to namespaced path", async () => {
    await updateCatalogSource("core", "my-ns", "src", { apiVersion: "varroa.dev/v1alpha1" as const, kind: "CatalogSource" as const, metadata: { name: "src" }, spec: { repoURL: "https://example.com" } });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources/my-ns/src");
    expect(opts.method).toBe("PUT");
  });

  it("deleteCatalogSource sends DELETE", async () => {
    await deleteCatalogSource("core", "my-ns", "src");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources/my-ns/src");
    expect(opts.method).toBe("DELETE");
  });

  it("syncCatalogSource POSTs to /sync path", async () => {
    await syncCatalogSource("core", "my-ns", "src");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogsources/my-ns/src/sync");
    expect(opts.method).toBe("POST");
  });
});

// ---- Catalog Items ----

describe("Catalog Items API", () => {
  it("listCatalogItems with no params", async () => {
    await listCatalogItems("core", {});
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogitems?");
  });

  it("listCatalogItems with all params", async () => {
    await listCatalogItems("core", { namespace: "ns", source: "src", type: "jcasc", q: "search" });
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toContain("namespace=ns");
    expect(url).toContain("source=src");
    expect(url).toContain("type=jcasc");
    expect(url).toContain("q=search");
  });

  it("getCatalogItem fetches by ns and name", async () => {
    await getCatalogItem("core", "my-ns", "item");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/catalogitems/my-ns/item");
  });
});

// ---- Composed Bundles ----

describe("Composed Bundle API", () => {
  it("listComposedBundles without namespace", async () => {
    await listComposedBundles("core");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles");
  });

  it("listComposedBundles with namespace query param", async () => {
    await listComposedBundles("core", "my-ns");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles?namespace=my-ns");
  });

  it("getComposedBundle fetches by ns and name", async () => {
    await getComposedBundle("core", "my-ns", "bundle");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/my-ns/bundle");
  });

  it("previewComposedBundle posts to the namespaced preview path", async () => {
    mockBffFetch.mockResolvedValue({ bundleYaml: "", jenkinsYaml: "", pluginsYaml: "", itemsYaml: "", rbacYaml: "", missing: [], drifted: [], warnings: [], unresolvedVariables: [] });
    await previewComposedBundle("core", "varroa-system", { displayName: "x" });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/varroa-system/preview");
    expect(opts.method).toBe("POST");
  });

  it("createComposedBundle POSTs to namespaced path", async () => {
    await createComposedBundle("core", "my-ns", { apiVersion: "varroa.dev/v1alpha1" as const, kind: "ComposedBundle" as const, metadata: { name: "b" }, spec: {} });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/my-ns");
    expect(opts.method).toBe("POST");
  });

  it("updateComposedBundle PUTs to namespaced path", async () => {
    await updateComposedBundle("core", "my-ns", "b", { apiVersion: "varroa.dev/v1alpha1" as const, kind: "ComposedBundle" as const, metadata: { name: "b" }, spec: {} });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/my-ns/b");
    expect(opts.method).toBe("PUT");
  });

  it("deleteComposedBundle sends DELETE", async () => {
    await deleteComposedBundle("core", "my-ns", "b");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/my-ns/b");
    expect(opts.method).toBe("DELETE");
  });

  it("validateComposedBundle sends the namespace as a query param", async () => {
    mockBffFetch.mockResolvedValue({ valid: true, errors: [], warnings: [] });
    await validateComposedBundle("core", "varroa-system", { displayName: "x" });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/validate?namespace=varroa-system");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ displayName: "x" });
  });

  it("validateComposedBundle url-encodes the namespace", async () => {
    mockBffFetch.mockResolvedValue({ valid: true, errors: [], warnings: [] });
    await validateComposedBundle("core", "ns/with space", { displayName: "x" });
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/composedbundles/validate?namespace=ns%2Fwith%20space");
  });
});

// ---- Provisioning Defaults ----

describe("Provisioning Defaults API", () => {
  it("getProvisioningConfig GETs the cluster-scoped provisioning config", async () => {
    await getProvisioningConfig("core");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/provisioning/config");
  });

  it("getProvisioningDefaults GETs the cluster-scoped singleton", async () => {
    await getProvisioningDefaults("core");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/provisioningdefaults/varroa-defaults");
  });
  it("updateProvisioningDefaults PUTs the config", async () => {
    const config = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "ProvisioningDefaults" as const, metadata: { name: "varroa-defaults" }, spec: {} };
    await updateProvisioningDefaults("core", config);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/provisioningdefaults/varroa-defaults");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual(config);
  });

  it("getVersionProfiles GETs the cluster-scoped list and unwraps items", async () => {
    mockBffFetch.mockResolvedValue({ items: [{ name: "jenkins-version-2-555" }] });
    const out = await getVersionProfiles("dev-cluster");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/dev-cluster/version-profiles");
    expect(out).toEqual([{ name: "jenkins-version-2-555" }]);
  });

  it("createVersionProfile POSTs the profile to the cluster-scoped collection", async () => {
    const profile = { metadata: { name: "jenkins-version-2-570" }, spec: {} };
    await createVersionProfile("core", profile);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/version-profiles");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(profile);
  });

  it("updateVersionProfile PUTs the named profile", async () => {
    const profile = { metadata: { name: "jenkins-version-2-570" }, spec: {} };
    await updateVersionProfile("core", "jenkins-version-2-570", profile);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/version-profiles/jenkins-version-2-570");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual(profile);
  });

  it("deleteVersionProfile DELETEs the named profile", async () => {
    await deleteVersionProfile("core", "jenkins-version-2-570");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/clusters/core/version-profiles/jenkins-version-2-570");
    expect(opts.method).toBe("DELETE");
  });
});

// ---- API Keys ----

describe("API Keys", () => {
  it("createApiKey POSTs /me/apikeys with an empty body", async () => {
    await createApiKey();
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/me/apikeys");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({});
  });

  it("createApiKey with expiresIn sends it in the body", async () => {
    await createApiKey("720h");
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ expiresIn: "720h" });
  });

  it("listApiKeys GETs /me/apikeys", async () => {
    await listApiKeys();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/me/apikeys");
  });

  it("revokeApiKey DELETEs /me/apikeys/{prefix}", async () => {
    await revokeApiKey("abc123");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/me/apikeys/abc123");
    expect(opts.method).toBe("DELETE");
  });

  it("rotateApiKey POSTs /me/apikeys/{prefix}/rotate", async () => {
    await rotateApiKey("abc123");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/me/apikeys/abc123/rotate");
    expect(opts.method).toBe("POST");
  });

  it("adminListUserApiKeys GETs /users/{user}/apikeys", async () => {
    await adminListUserApiKeys("nathan");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/users/nathan/apikeys");
  });

  it("adminRevokeApiKey DELETEs /users/{user}/apikeys/{prefix}", async () => {
    await adminRevokeApiKey("nathan", "abc123");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/users/nathan/apikeys/abc123");
    expect(opts.method).toBe("DELETE");
  });

  it("url-encodes the prefix in revokeApiKey", async () => {
    await revokeApiKey("abc/def");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/me/apikeys/abc%2Fdef");
  });
});

// ---- parsePreflightChecks ----

describe("parsePreflightChecks", () => {
  it("extracts checks[] from a structured ApiError body", () => {
    const err = { status: 400, message: "Preflight checks failed.", body: { checks: [{ id: "version", status: "fail", message: "too old" }] } };
    const checks = parsePreflightChecks(err);
    expect(checks).toEqual([{ id: "version", status: "fail", message: "too old" }]);
  });

  it("returns null for a non-preflight error message (no JSON body)", () => {
    expect(parsePreflightChecks(new Error("500 Internal Server Error: boom"))).toBeNull();
  });

  it("returns null for an error body without a checks array", () => {
    expect(parsePreflightChecks({ status: 400, message: "Bad request", body: { error: "nope" } })).toBeNull();
  });

  it("returns null for malformed JSON in the message", () => {
    expect(parsePreflightChecks(new Error('400 Bad Request: {"checks": [oops}'))).toBeNull();
  });

  it("handles non-Error inputs by stringifying", () => {
    expect(parsePreflightChecks("just a string")).toBeNull();
  });
});

// ---- Brood operations ----

describe("Brood operations API", () => {
  it("listBroodOperations without namespace", async () => {
    await listBroodOperations();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations");
  });

  it("listBroodOperations with namespace query param", async () => {
    await listBroodOperations("team-a");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations?namespace=team-a");
  });

  it("getBroodOperation encodes namespace and name", async () => {
    await getBroodOperation("broodop-restart-abc", "team a");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations/team%20a/broodop-restart-abc");
  });

  it("createBroodOperation POSTs the request body", async () => {
    const body = { namespace: "team-a", spec: { action: { verb: "reconcile" as const }, targets: { names: ["ctrl-a"] } } };
    await createBroodOperation(body);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(body);
  });

  it("deleteBroodOperation sends DELETE to the run path", async () => {
    await deleteBroodOperation("broodop-stop-xyz", "team-a");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations/team-a/broodop-stop-xyz");
    expect(opts.method).toBe("DELETE");
  });

  it("suspendBroodOperation POSTs the suspend body", async () => {
    await suspendBroodOperation("broodop-stop-xyz", "team-a", true);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations/team-a/broodop-stop-xyz/suspend");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ suspend: true });
  });

  it("previewBroodOperation POSTs to the preview path", async () => {
    const body = { spec: { action: { verb: "restart" as const }, targets: { selector: { matchLabels: { tier: "prod" } } } } };
    await previewBroodOperation(body);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/brood-operations/preview");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(body);
  });

  it("broodStreamUrl builds the per-run SSE path with encoding", () => {
    const url = broodStreamUrl("broodop-restart-abc", "team a");
    expect(url).toContain("/brood-operations/team%20a/broodop-restart-abc/stream");
  });
});

// ---- Restored coverage for core-local functions unchanged by remote-config-authoring ----
describe("VarroaRole API", () => {
  it("listRoles GETs /roles", async () => {
    await listRoles();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/roles");
  });

  it("getRole encodes the name", async () => {
    await getRole("role with spaces");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/roles/role%20with%20spaces");
  });

  it("createRole POSTs the role", async () => {
    const role = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "VarroaRole" as const, metadata: { name: "r" }, spec: {} };
    await createRole(role);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/roles");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(role);
  });

  it("updateRole PUTs the role", async () => {
    const role = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "VarroaRole" as const, metadata: { name: "r" }, spec: {} };
    await updateRole("r", role);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/roles/r");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual(role);
  });

  it("deleteRole sends DELETE", async () => {
    await deleteRole("r");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/roles/r");
    expect(opts.method).toBe("DELETE");
  });
});

// ---- VarroaRoleBinding CRUD ----

describe("VarroaRoleBinding API", () => {
  it("listRoleBindings GETs /rolebindings", async () => {
    await listRoleBindings();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/rolebindings");
  });

  it("getRoleBinding encodes the name", async () => {
    await getRoleBinding("rb name");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/rolebindings/rb%20name");
  });

  it("createRoleBinding POSTs the binding", async () => {
    const binding = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "VarroaRoleBinding" as const, metadata: { name: "rb" }, spec: { subjects: [], roleRef: "r" } };
    await createRoleBinding(binding);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/rolebindings");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual(binding);
  });

  it("updateRoleBinding PUTs the binding", async () => {
    const binding = { apiVersion: "varroa.dev/v1alpha1" as const, kind: "VarroaRoleBinding" as const, metadata: { name: "rb" }, spec: { subjects: [], roleRef: "r" } };
    await updateRoleBinding("rb", binding);
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/rolebindings/rb");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body)).toEqual(binding);
  });

  it("deleteRoleBinding sends DELETE", async () => {
    await deleteRoleBinding("rb");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/rolebindings/rb");
    expect(opts.method).toBe("DELETE");
  });

});

describe("Permissions API", () => {
  it("getMyPermissions GETs /me/permissions", async () => {
    await getMyPermissions();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/me/permissions");
  });
});

describe("API Keys (restored)", () => {
  it("url-encodes the username in adminListUserApiKeys", async () => {
    await adminListUserApiKeys("user name");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/users/user%20name/apikeys");
  });
});

describe("Brood operations (restored)", () => {
  it("suspendBroodOperation can resume", async () => {
    await suspendBroodOperation("broodop-stop-xyz", "team-a", false);
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body)).toEqual({ suspend: false });
  });
});

// ---- Pipelines ----

describe("Pipelines API", () => {
  it("listPipelines GETs /pipelines/{team}", async () => {
    mockBffFetch.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    await listPipelines("payments");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/pipelines/payments");
  });

  it("getPipelineJob percent-encodes a job path containing a slash as one segment (D2)", async () => {
    mockBffFetch.mockResolvedValue({ jobPath: "payments/checkout-api/main", health: "unknown" });
    await getPipelineJob("payments", "payments/checkout-api/main");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/pipelines/payments/jobs/payments%2Fcheckout-api%2Fmain");
  });

  it("listRuns builds the run-list query parameters", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listRuns("payments", "checkout-api/main", {
      branch: "main",
      result: "FAILURE",
      actor: "nathan",
      cause: "push",
      limit: 25,
      cursor: "abc123",
    });
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe(
      "/pipelines/payments/jobs/checkout-api%2Fmain/runs" +
        "?branch=main&result=FAILURE&actor=nathan&cause=push&limit=25&cursor=abc123",
    );
  });

  it("listRuns omits the query string when no params are given", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listRuns("payments", "checkout-api/main");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/pipelines/payments/jobs/checkout-api%2Fmain/runs");
  });

  it("createRun POSTs to the runs collection and reads the 202 JSON body via bffFetchText", async () => {
    mockBffFetchText.mockResolvedValue(
      JSON.stringify({ runId: "01912a3b-0000-7000-8000-000000000000", intentKey: "manual:01912a3b-0000-7000-8000-000000000000", state: "requested" }),
    );
    const result = await createRun("payments", "checkout-api/main", { clientToken: "01912a3b-0000-7000-8000-000000000000" });
    const [url, opts] = mockBffFetchText.mock.calls[0];
    expect(url).toBe("/pipelines/payments/jobs/checkout-api%2Fmain/runs");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ clientToken: "01912a3b-0000-7000-8000-000000000000" });
    expect(result).toEqual({
      runId: "01912a3b-0000-7000-8000-000000000000",
      intentKey: "manual:01912a3b-0000-7000-8000-000000000000",
      state: "requested",
    });
  });

  it("createRun sends no body when none is given", async () => {
    mockBffFetchText.mockResolvedValue(JSON.stringify({ runId: "r1", intentKey: "manual:r1", state: "requested" }));
    await createRun("payments", "checkout-api/main");
    const [, opts] = mockBffFetchText.mock.calls[0];
    expect(opts.body).toBeUndefined();
  });

  it("getRun GETs /runs/{run}", async () => {
    mockBffFetch.mockResolvedValue({ runId: "r1", jobPath: "checkout-api/main", status: "SUCCESS" });
    await getRun("r1");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/runs/r1");
  });

  it("runLogUrl builds the SSE stream URL with from/fromLine", () => {
    const url = runLogUrl("r1", { from: 8192, fromLine: 461 });
    expect(url).toBe("/api/v1/runs/r1/log?from=8192&fromLine=461");
  });

  it("runLogUrl builds the raw plain-text URL", () => {
    const url = runLogUrl("r1", { raw: true });
    expect(url).toBe("/api/v1/runs/r1/log?raw=true");
  });

  it("runLogUrl with no params carries no query string", () => {
    const url = runLogUrl("r1");
    expect(url).toBe("/api/v1/runs/r1/log");
  });

  it("listRunArtifacts GETs /runs/{run}/artifacts", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listRunArtifacts("r1");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/runs/r1/artifacts");
  });

  it("downloadRunArtifact percent-encodes each segment of a multi-segment path", async () => {
    // A string body keeps the test out of the jsdom-versus-Node Blob realm
    // split: Response.blob() returns Node's Blob, so an instanceof check
    // against the jsdom global fails on some Node versions.
    const fetchMock = vi.fn().mockResolvedValue(new Response("bytes", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    mockGetToken.mockReturnValue("test-token");

    const result = await downloadRunArtifact("r1", "target/site with spaces/report.html");

    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/runs/r1/artifacts/target/site%20with%20spaces/report.html");
    expect(opts.headers.Authorization).toBe("Bearer test-token");
    expect(Object.prototype.toString.call(result)).toBe("[object Blob]");
    expect(result.size).toBe(5);
    vi.unstubAllGlobals();
  });

  it("downloadRunArtifact throws ApiError on a non-ok response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("nope", { status: 409 })));
    await expect(downloadRunArtifact("r1", "report.html")).rejects.toMatchObject({ status: 409 });
    vi.unstubAllGlobals();
  });

  it("listRunTests GETs /runs/{run}/tests", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listRunTests("r1");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/runs/r1/tests");
  });

  it("getTestCaseHistory encodes the job path and the case key as one segment each", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await getTestCaseHistory("payments", "checkout-api/main", "a%2Fb/c");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/pipelines/payments/jobs/checkout-api%2Fmain/tests/a%252Fb%2Fc");
  });

  it("getTestCaseHistory sends the limit when one is given", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await getTestCaseHistory("payments", "main", "Foo/bar", 20);
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/pipelines/payments/jobs/main/tests/Foo%2Fbar?limit=20");
  });

  it("listRunChanges GETs /runs/{run}/changes", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listRunChanges("r1");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/runs/r1/changes");
  });

  it("cancelRun POSTs /runs/{run}/cancel and reads the 202 JSON body", async () => {
    mockBffFetchText.mockResolvedValue(JSON.stringify({ state: "requested" }));
    const result = await cancelRun("r1");
    const [url, opts] = mockBffFetchText.mock.calls[0];
    expect(url).toBe("/runs/r1/cancel");
    expect(opts.method).toBe("POST");
    expect(result).toEqual({ state: "requested" });
  });

  it("rerunRun POSTs /runs/{run}/rerun and reads the 202 JSON body", async () => {
    mockBffFetchText.mockResolvedValue(JSON.stringify({ runId: "r2", intentKey: "retry:r1", state: "requested" }));
    const result = await rerunRun("r1");
    const [url, opts] = mockBffFetchText.mock.calls[0];
    expect(url).toBe("/runs/r1/rerun");
    expect(opts.method).toBe("POST");
    expect(result).toEqual({ runId: "r2", intentKey: "retry:r1", state: "requested" });
  });
});

// ---- Teams ----

describe("Teams API", () => {
  it("listTeams GETs /teams and unwraps items", async () => {
    mockBffFetch.mockResolvedValue({ items: [{ name: "payments", namespaces: ["ns1"] }] });
    const result = await listTeams();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/teams");
    expect(result).toEqual([{ name: "payments", namespaces: ["ns1"] }]);
  });

  it("getTeam GETs /teams/{name} and reports the seven pipelines ledger settings and status.rehydration", async () => {
    mockBffFetch.mockResolvedValue({
      name: "payments",
      namespaces: ["ns1"],
      pipelines: {
        profile: "lts",
        rehydrateBuilds: 5,
        rehydrateDeadline: "45m0s",
        interruptedRetry: "automatic",
        concurrency: "cancelInProgress",
        cronBackstop: false,
        cronCatchUp: "none",
        cronGrace: "10m0s",
      },
      rehydration: {
        generation: 3,
        cellInstance: "pod-uid-1",
        records: 12,
        truncated: true,
        unseededJobs: 2,
        deferred: ["payments/api/main"],
        attempts: 4,
        lastAttempt: "2026-09-10T12:00:00Z",
      },
    });
    const result = await getTeam("payments");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/teams/payments");
    expect(result.pipelines).toEqual({
      profile: "lts",
      rehydrateBuilds: 5,
      rehydrateDeadline: "45m0s",
      interruptedRetry: "automatic",
      concurrency: "cancelInProgress",
      cronBackstop: false,
      cronCatchUp: "none",
      cronGrace: "10m0s",
    });
    expect(result.rehydration).toEqual({
      generation: 3,
      cellInstance: "pod-uid-1",
      records: 12,
      truncated: true,
      unseededJobs: 2,
      deferred: ["payments/api/main"],
      attempts: 4,
      lastAttempt: "2026-09-10T12:00:00Z",
    });
  });

  it("createTeam POSTs the seven pipelines ledger settings in the request body", async () => {
    mockBffFetch.mockResolvedValue({ name: "payments" });
    await createTeam({
      name: "payments",
      namespaces: ["ns1"],
      members: ["alice"],
      pipelines: {
        rehydrateBuilds: 20,
        rehydrateDeadline: "1h0m0s",
        interruptedRetry: "automatic",
        concurrency: "cancelInProgress",
        cronBackstop: false,
        cronCatchUp: "none",
        cronGrace: "15m0s",
      },
    });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/teams");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body).pipelines).toEqual({
      rehydrateBuilds: 20,
      rehydrateDeadline: "1h0m0s",
      interruptedRetry: "automatic",
      concurrency: "cancelInProgress",
      cronBackstop: false,
      cronCatchUp: "none",
      cronGrace: "15m0s",
    });
  });

  it("updateTeam PUTs to /teams/{name} with a JSON body", async () => {
    mockBffFetch.mockResolvedValue({ name: "payments" });
    await updateTeam("payments", {
      name: "payments",
      namespaces: ["ns1"],
      members: ["alice"],
      pipelines: { concurrency: "allow" },
    });
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/teams/payments");
    expect(opts.method).toBe("PUT");
    expect(JSON.parse(opts.body).pipelines).toEqual({ concurrency: "allow" });
  });

  it("deleteTeam DELETEs /teams/{name}", async () => {
    mockBffFetch.mockResolvedValue(undefined);
    await deleteTeam("payments");
    const [url, opts] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/teams/payments");
    expect(opts.method).toBe("DELETE");
  });

  it("getTeam reports pipelines.exports with both a job grant and a locks grant", async () => {
    mockBffFetch.mockResolvedValue({
      name: "payments",
      namespaces: ["ns1"],
      pipelines: {
        profile: "lts",
        exports: [
          { job: "release/*", callers: ["payments", "search"] },
          { locks: ["deploy-prod", "staging/*"], callers: ["payments"] },
        ],
      },
    });
    const result = await getTeam("payments");
    expect(result.pipelines?.exports).toEqual([
      { job: "release/*", callers: ["payments", "search"] },
      { locks: ["deploy-prod", "staging/*"], callers: ["payments"] },
    ]);
  });

  it("updateTeam PUTs pipelines.exports in the request body unchanged", async () => {
    mockBffFetch.mockResolvedValue({ name: "payments" });
    await updateTeam("payments", {
      name: "payments",
      namespaces: ["ns1"],
      members: ["alice"],
      pipelines: {
        exports: [{ job: "release/*", callers: ["search"] }],
      },
    });
    const [, opts] = mockBffFetch.mock.calls[0];
    expect(JSON.parse(opts.body).pipelines.exports).toEqual([
      { job: "release/*", callers: ["search"] },
    ]);
  });
});

// ---- Runners ----

describe("Runners API", () => {
  it("listRunners GETs /runners and unwraps items", async () => {
    mockBffFetch.mockResolvedValue({
      items: [
        {
          name: "win-1",
          spec: { team: "payments", launcher: "ssh", remoteFS: "C:\\J" },
          status: { connected: true },
        },
      ],
    });
    const result = await listRunners();
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/runners");
    expect(result).toEqual([
      {
        name: "win-1",
        spec: { team: "payments", launcher: "ssh", remoteFS: "C:\\J" },
        status: { connected: true },
      },
    ]);
  });

  it("listRunners GETs /runners?team=<name> when given a team", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    await listRunners("payments");
    const [url] = mockBffFetch.mock.calls[0];
    expect(url).toBe("/runners?team=payments");
  });

  it("listRunners never reads status.token off the result it returns to a caller", async () => {
    mockBffFetch.mockResolvedValue({
      items: [
        {
          name: "agent-1",
          spec: { team: "payments", launcher: "inbound", remoteFS: "/home/jenkins" },
          status: { connected: false },
        },
      ],
    });
    const result = await listRunners("payments");
    expect(result[0].status.token).toBeUndefined();
  });
});
