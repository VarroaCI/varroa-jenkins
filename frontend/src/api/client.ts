import type {
  AssistantSession,
  AssistantSessionContext,
  AssistantSessionDetail,
  AssistantSessionList,
  AssistantApprovalList,
  AssistantApprovalOutcome,
  AssistantAcceptedState,
  AssistantSettings,
  AssistantSettingsUpdate,
  AssistantProviderList,
  MyAssistant,
  MyAssistantUpdate,
  MyAssistantKeyRequest,
  CreateRunNoteRequest,
  RunNote,
  RunNoteList,
} from "../types/assistant";
import type {
  Controller,
  ControllerList,
  ControllerDiff,
  ControllerClass,
  ProbeSpec,
  ProbesSpec,
  ClusterEntry,
  ProvisioningDefaults,
  ProvisioningConfig,
  PreflightCheck,
  CatalogSource,
  CatalogSourceList,
  CatalogItemDetailResponse,
  CatalogItemList,
  ComposedBundle,
  ComposedBundleList,
  ComposedBundlePreview,
  ComposedBundleSpec,
  VarroaRole,
  VarroaRoleList,
  VarroaRoleBinding,
  VarroaRoleBindingList,
  JenkinsRole,
  JenkinsRoleList,
  JenkinsRoleBinding,
  JenkinsRoleBindingList,
  KeyMeta,
  PreviewRequest,
  PreviewResponse,
  VersionProfileDetail,
  VersionCandidate,
  VersionCandidateList,
  SubjectRef,
  BroodRun,
  BroodListResponse,
  BroodPreviewResponse,
  CreateBroodRequest,
  BroodActionResponse,
  BroodSchedule,
  BroodScheduleListResponse,
  CreateBroodScheduleRequest,
  UpdateCenterStatus,
  UpdateCenterPlugins,
  UpdateCenterUploadResult,
  FleetPluginsRollup,
  FleetPluginDrilldown,
  PipelineListResponse,
  PipelineJobDetail,
  JobBranchList,
  ScanRequestedResponse,
  RunListResponse,
  CreateRunRequest,
  RunActionResponse,
  CancelRunResponse,
  RunDetail,
  RunArtifactListResponse,
  RunTestListResponse,
  RunRestart,
  RunScript,
  TestCaseHistoryResponse,
  RunChangeListResponse,
  RunLinkPage,
  TeamPipelines,
  TeamPlacement,
  TeamRehydration,
  RunnerEntry,
  CellUsage,
  CellUsageListResponse,
  TeamCell,
  CellPool,
  CellPoolListResponse,
  CellPoolScaleRequest,
  CellGrants,
  FleetLockListResponse,
  FleetLockReleaseRequest,
  FleetLockReleaseResponse,
  PipelineTeamList,
  ProposePipelineResponse,
} from "../types";
import type { ControllerDetail } from "../hooks/useControllers";
import type {
  TeamAgent,
  TeamAgentCapabilities,
  TeamAgentItemList,
  TeamAgentSessionList,
  TeamAgentUnattendedResult,
} from "../types/teamAgent";
import type { Permissions } from "../types/auth";
import { ApiError, bffFetch, bffFetchText, bffUpload, getToken } from "../hooks/useApi";

export const BFF_BASE = import.meta.env.VITE_VARROA_BFF_URL || "/api/v1";

/**
 * Info about a single SSA field conflict returned by the API on 409 Conflict.
 */
export type ConflictInfo = {
  field: string;
  manager?: string;
  message: string;
};

/**
 * Thrown when a PATCH update receives a 409 field-ownership conflict.
 * The `conflicts` array mirrors the OpenAPI ConflictResponse body.
 */
export class ControllerConflictError extends Error {
  constructor(
    public readonly conflicts: ConflictInfo[],
    message?: string,
  ) {
    super(message || "Field ownership conflict");
    this.name = "ControllerConflictError";
  }
}

// ---- Path helpers ----

const enc = (s: string) => encodeURIComponent(s);

function isEmptyProbeSpec(spec: ProbeSpec | undefined): boolean {
  if (!spec) return true;
  return (
    spec.disabled !== true &&
    spec.initialDelaySeconds == null &&
    spec.periodSeconds == null &&
    spec.timeoutSeconds == null &&
    spec.failureThreshold == null &&
    spec.successThreshold == null
  );
}

function pruneEmptyProbes(body: Record<string, unknown>): void {
  const spec = body.spec as Record<string, unknown> | undefined;
  if (!spec) return;
  const probes = spec.probes as ProbesSpec | undefined;
  if (!probes) return;

  const cleaned: ProbesSpec = {};
  for (const key of ["startup", "readiness", "liveness"] as const) {
    const probe = probes[key];
    if (!isEmptyProbeSpec(probe)) {
      cleaned[key] = probe;
    }
  }

  if (Object.keys(cleaned).length > 0) {
    spec.probes = cleaned;
  } else {
    delete spec.probes;
  }
}

/** Build a cluster-scoped controller path. */
const ctrlBase = (cluster: string, ns: string, name?: string) =>
  `/clusters/${enc(cluster)}/controllers/${enc(ns)}` +
  (name != null ? `/${enc(name)}` : "");

// ---- Controllers ----

export function listControllers(namespace?: string): Promise<ControllerList> {
  const qs = namespace ? `?namespace=${encodeURIComponent(namespace)}` : "";
  return bffFetch<ControllerList>(`/controllers${qs}`);
}

export function getController(cluster: string, name: string, namespace: string): Promise<Controller> {
  return bffFetch<Controller>(ctrlBase(cluster, namespace, name));
}

export function createController(
  cluster: string,
  namespace: string,
  spec: Partial<Controller> & { metadata: { name: string }; bundle?: ComposedBundleSpec },
): Promise<Controller> {
  const { bundle, ...controllerFields } = spec;
  const body: Record<string, unknown> = {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "Controller",
    ...controllerFields,
  };
  pruneEmptyProbes(body);
  if (bundle) {
    (body as Record<string, unknown>)["bundle"] = bundle;
  }
  return bffFetch<Controller>(ctrlBase(cluster, namespace), {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function deleteController(cluster: string, name: string, namespace: string): Promise<void> {
  return bffFetch<void>(ctrlBase(cluster, namespace, name), { method: "DELETE" });
}

export function preflightController(
  cluster: string,
  namespace: string,
  body: Record<string, unknown>,
): Promise<{ checks: PreflightCheck[] }> {
  return bffFetch<{ checks: PreflightCheck[] }>(
    `${ctrlBase(cluster, namespace)}/preflight`,
    {
      method: "POST",
      body: JSON.stringify(body),
    }
  );
}

export function renderController(
  cluster: string,
  namespace: string,
  body: Record<string, unknown>,
): Promise<string> {
  return bffFetchText(
    `${ctrlBase(cluster, namespace)}/render`,
    {
      method: "POST",
      headers: { Accept: "application/yaml" },
      body: JSON.stringify(body),
    }
  );
}

export function controllerEventsUrl(cluster: string, ns: string, name: string): string {
  return `${BFF_BASE}${ctrlBase(cluster, ns, name)}/events`;
}

export function updateController(
  cluster: string,
  name: string,
  namespace: string,
  patch: Record<string, unknown>,
  opts?: { force?: boolean },
): Promise<ControllerDetail> {
  pruneEmptyProbes(patch);
  const url = opts?.force
    ? `${ctrlBase(cluster, namespace, name)}?force=true`
    : ctrlBase(cluster, namespace, name);
  return bffFetch<ControllerDetail>(url, {
    method: "PATCH",
    headers: { "Content-Type": "application/merge-patch+json" },
    body: JSON.stringify(patch),
  }).catch((err: unknown) => {
    if (err instanceof ApiError && err.status === 409) {
      const body = err.body as Record<string, unknown> | undefined;
      const rawConflicts = body?.conflicts;
      const conflicts: ConflictInfo[] = Array.isArray(rawConflicts)
        ? rawConflicts.map((c: Record<string, unknown>) => ({
            field: String(c.field ?? ""),
            manager: c.manager != null ? String(c.manager) : undefined,
            message: String(c.message ?? ""),
          }))
        : [];
      throw new ControllerConflictError(conflicts, String(err.message));
    }
    throw err;
  });
}

// Approve restart. expectedContentHash is the contentHash of the pending
// change that the user reviewed; the BFF refuses with 409 when the pending
// change has another hash.
export function approveRestart(
  cluster: string,
  ns: string,
  name: string,
  action: "reload" | "restart",
  expectedContentHash?: string,
): Promise<{ status: string; action: string }> {
  return bffFetch<{ status: string; action: string }>(
    `${ctrlBase(cluster, ns, name)}/approve`,
    { method: "POST", body: JSON.stringify(expectedContentHash ? { action, expectedContentHash } : { action }) }
  );
}

/**
 * pendingChangeChanged answers the sections of the new pending change when
 * an approval failed because the pending change changed, and null for any
 * other error.
 */
export function pendingChangeChanged(e: unknown): string[] | null {
  if (!(e instanceof ApiError) || e.status !== 409) return null;
  const body = e.body;
  if (!body || typeof body !== "object" || !("changes" in body) || !Array.isArray(body.changes)) return null;
  return body.changes.filter((c): c is string => typeof c === "string");
}

// Approve item deletion
export function approveDeletion(
  cluster: string,
  ns: string,
  name: string,
  path: string,
): Promise<{ status: string; path: string }> {
  return bffFetch<{ status: string; path: string }>(
    `${ctrlBase(cluster, ns, name)}/approve-deletion`,
    { method: "POST", body: JSON.stringify({ path }) }
  );
}

// Admin controls
export const reprovisionController = (cluster: string, name: string, ns: string) =>
  bffFetch(`${ctrlBase(cluster, ns, name)}/reprovision`, { method: "POST" });

export const restartController = (cluster: string, name: string, ns: string) =>
  bffFetch(`${ctrlBase(cluster, ns, name)}/restart`, { method: "POST" });

export const setPowerState = (cluster: string, name: string, ns: string, state: "Running" | "Stopped") =>
  updateController(cluster, name, ns, { spec: { powerState: state } });

// Hibernate and wake are POST action endpoints, not power-state writes: the
// operator records hibernation in status.hibernated, never in spec.powerState.
export const hibernateController = (cluster: string, name: string, ns: string) =>
  bffFetch<{ status: string }>(`${ctrlBase(cluster, ns, name)}/hibernate`, { method: "POST" });

export const wakeController = (cluster: string, name: string, ns: string) =>
  bffFetch<{ status: string }>(`${ctrlBase(cluster, ns, name)}/wake`, { method: "POST" });

export const updateIngress = (cluster: string, name: string, ns: string, ingressSpec: Record<string, unknown>) =>
  updateController(cluster, name, ns, { spec: { ingressSpec } });

// ---- Clusters ----

export function listClusters(): Promise<ClusterEntry[]> {
  return bffFetch<{items: ClusterEntry[]}>("/clusters").then(r => r.items);
}

export function drainCluster(name: string, confirm: string): Promise<{ state: string }> {
  return bffFetch<{ state: string }>(`/clusters/${enc(name)}/drain`, {
    method: "POST",
    body: JSON.stringify({ confirm }),
  });
}

export function cancelClusterDrain(name: string): Promise<{ state: string }> {
  return bffFetch<{ state: string }>(`/clusters/${enc(name)}/drain`, {
    method: "DELETE",
  });
}

// VarroaRole CRUD
export function listRoles(): Promise<VarroaRoleList> {
  return bffFetch<VarroaRoleList>("/roles");
}

export function getRole(name: string): Promise<VarroaRole> {
  return bffFetch<VarroaRole>(`/roles/${encodeURIComponent(name)}`);
}

export function createRole(role: VarroaRole): Promise<VarroaRole> {
  return bffFetch<VarroaRole>("/roles", {
    method: "POST",
    body: JSON.stringify(role),
  });
}

export function updateRole(name: string, role: VarroaRole): Promise<VarroaRole> {
  return bffFetch<VarroaRole>(`/roles/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(role),
  });
}

export function deleteRole(name: string): Promise<void> {
  return bffFetch<void>(`/roles/${encodeURIComponent(name)}`, { method: "DELETE" });
}

// VarroaRoleBinding CRUD
export function listRoleBindings(): Promise<VarroaRoleBindingList> {
  return bffFetch<VarroaRoleBindingList>("/rolebindings");
}

export function getRoleBinding(name: string): Promise<VarroaRoleBinding> {
  return bffFetch<VarroaRoleBinding>(`/rolebindings/${encodeURIComponent(name)}`);
}

export function createRoleBinding(binding: VarroaRoleBinding): Promise<VarroaRoleBinding> {
  return bffFetch<VarroaRoleBinding>("/rolebindings", {
    method: "POST",
    body: JSON.stringify(binding),
  });
}

export function updateRoleBinding(name: string, binding: VarroaRoleBinding): Promise<VarroaRoleBinding> {
  return bffFetch<VarroaRoleBinding>(`/rolebindings/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(binding),
  });
}

export function deleteRoleBinding(name: string): Promise<void> {
  return bffFetch<void>(`/rolebindings/${encodeURIComponent(name)}`, { method: "DELETE" });
}

// JenkinsRole CRUD (cluster-scoped)
export function listJenkinsRoles(cluster: string): Promise<JenkinsRoleList> {
  return bffFetch<JenkinsRoleList>(`/clusters/${enc(cluster)}/jenkinsroles`);
}

export function getJenkinsRole(cluster: string, name: string): Promise<JenkinsRole> {
  return bffFetch<JenkinsRole>(`/clusters/${enc(cluster)}/jenkinsroles/${encodeURIComponent(name)}`);
}

export function createJenkinsRole(cluster: string, role: JenkinsRole): Promise<JenkinsRole> {
  return bffFetch<JenkinsRole>(`/clusters/${enc(cluster)}/jenkinsroles`, {
    method: "POST",
    body: JSON.stringify(role),
  });
}

export function updateJenkinsRole(cluster: string, name: string, role: JenkinsRole): Promise<JenkinsRole> {
  return bffFetch<JenkinsRole>(`/clusters/${enc(cluster)}/jenkinsroles/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(role),
  });
}

export function deleteJenkinsRole(cluster: string, name: string): Promise<void> {
  return bffFetch<void>(`/clusters/${enc(cluster)}/jenkinsroles/${encodeURIComponent(name)}`, { method: "DELETE" });
}

// JenkinsRoleBinding CRUD (cluster-scoped)
export function listJenkinsRoleBindings(cluster: string): Promise<JenkinsRoleBindingList> {
  return bffFetch<JenkinsRoleBindingList>(`/clusters/${enc(cluster)}/jenkinsrolebindings`);
}

export function getJenkinsRoleBinding(cluster: string, name: string): Promise<JenkinsRoleBinding> {
  return bffFetch<JenkinsRoleBinding>(`/clusters/${enc(cluster)}/jenkinsrolebindings/${encodeURIComponent(name)}`);
}

export function createJenkinsRoleBinding(cluster: string, binding: JenkinsRoleBinding): Promise<JenkinsRoleBinding> {
  return bffFetch<JenkinsRoleBinding>(`/clusters/${enc(cluster)}/jenkinsrolebindings`, {
    method: "POST",
    body: JSON.stringify(binding),
  });
}

export function updateJenkinsRoleBinding(
  cluster: string,
  name: string,
  binding: JenkinsRoleBinding
): Promise<JenkinsRoleBinding> {
  return bffFetch<JenkinsRoleBinding>(`/clusters/${enc(cluster)}/jenkinsrolebindings/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(binding),
  });
}

export function deleteJenkinsRoleBinding(cluster: string, name: string): Promise<void> {
  return bffFetch<void>(`/clusters/${enc(cluster)}/jenkinsrolebindings/${encodeURIComponent(name)}`, { method: "DELETE" });
}

// Permissions
export function getMyPermissions(): Promise<Permissions> {
  return bffFetch<Permissions>("/me/permissions");
}

// ---- Catalog Sources ----
export function listCatalogSources(cluster: string, namespace?: string): Promise<CatalogSourceList> {
  const qs = namespace ? `?namespace=${encodeURIComponent(namespace)}` : "";
  return bffFetch<CatalogSourceList>(`/clusters/${enc(cluster)}/catalogsources${qs}`);
}

export function getCatalogSource(cluster: string, name: string, namespace: string): Promise<CatalogSource> {
  return bffFetch<CatalogSource>(`/clusters/${enc(cluster)}/catalogsources/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`);
}

export function createCatalogSource(cluster: string, ns: string, src: CatalogSource): Promise<CatalogSource> {
  return bffFetch<CatalogSource>(`/clusters/${enc(cluster)}/catalogsources/${encodeURIComponent(ns)}`, { method: "POST", body: JSON.stringify(src) });
}

export function updateCatalogSource(cluster: string, ns: string, name: string, src: CatalogSource): Promise<CatalogSource> {
  return bffFetch<CatalogSource>(`/clusters/${enc(cluster)}/catalogsources/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify(src) });
}

export function deleteCatalogSource(cluster: string, ns: string, name: string): Promise<void> {
  return bffFetch<void>(`/clusters/${enc(cluster)}/catalogsources/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export function syncCatalogSource(cluster: string, ns: string, name: string): Promise<void> {
  return bffFetch<void>(`/clusters/${enc(cluster)}/catalogsources/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/sync`, { method: "POST" });
}

// ---- Catalog Items ----
export function listCatalogItems(cluster: string, params: { namespace?: string; source?: string; type?: string; q?: string }): Promise<CatalogItemList> {
  const qs = new URLSearchParams();
  if (params.namespace) qs.set("namespace", params.namespace);
  if (params.source) qs.set("source", params.source);
  if (params.type) qs.set("type", params.type);
  if (params.q) qs.set("q", params.q);
  return bffFetch<CatalogItemList>(`/clusters/${enc(cluster)}/catalogitems?${qs}`);
}

/**
 * Fetches one catalog item. The response wraps the item alongside a per-profile
 * lock-pin projection the BFF joins from the JenkinsVersionProfile locks at read
 * time — the pins are cluster state and would go stale if stored on the item.
 */
export function getCatalogItem(
  cluster: string,
  ns: string,
  name: string,
): Promise<CatalogItemDetailResponse> {
  return bffFetch<CatalogItemDetailResponse>(
    `/clusters/${enc(cluster)}/catalogitems/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
  );
}

// ---- Composed Bundles ----
export function listComposedBundles(cluster: string, namespace?: string): Promise<ComposedBundleList> {
  const qs = namespace ? `?namespace=${encodeURIComponent(namespace)}` : "";
  return bffFetch<ComposedBundleList>(`/clusters/${enc(cluster)}/composedbundles${qs}`);
}

export function getComposedBundle(cluster: string, ns: string, name: string): Promise<ComposedBundle> {
  return bffFetch<ComposedBundle>(`/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`);
}

export async function previewComposedBundle(cluster: string, ns: string, spec: ComposedBundleSpec): Promise<ComposedBundlePreview> {
  // The Go BFF serializes nil slices as JSON null (missing/drifted/warnings have
  // no omitempty). Normalize to [] so the declared string[] types hold and
  // consumers can read .length / .map without crashing on an empty bundle.
  const p = await bffFetch<ComposedBundlePreview>(`/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}/preview`, { method: "POST", body: JSON.stringify(spec) });
  return {
    ...p,
    missing: p.missing ?? [],
    drifted: p.drifted ?? [],
    warnings: p.warnings ?? [],
  };
}

export function createComposedBundle(cluster: string, ns: string, bundle: ComposedBundle): Promise<ComposedBundle> {
  return bffFetch<ComposedBundle>(`/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}`, { method: "POST", body: JSON.stringify(bundle) });
}

export function updateComposedBundle(cluster: string, ns: string, name: string, bundle: ComposedBundle): Promise<ComposedBundle> {
  return bffFetch<ComposedBundle>(`/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify(bundle) });
}

export function deleteComposedBundle(cluster: string, ns: string, name: string): Promise<void> {
  return bffFetch<void>(`/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export interface ValidateComposedBundleResult {
  valid: boolean;
  errors: string[];
  warnings: string[];
}

export async function validateComposedBundle(cluster: string, ns: string, spec: ComposedBundleSpec): Promise<ValidateComposedBundleResult> {
  // The namespace is required so the backend resolves catalog itemRefs and git
  // secretRefs in the same namespace the bundle will live in.
  const r = await bffFetch<ValidateComposedBundleResult>(
    `/clusters/${enc(cluster)}/composedbundles/validate?namespace=${encodeURIComponent(ns)}`,
    { method: "POST", body: JSON.stringify(spec) },
  );
  // errors/warnings are omitempty on the BFF response and absent on a clean
  // validation; normalize to [] so the result is safe to render.
  return { ...r, errors: r.errors ?? [], warnings: r.warnings ?? [] };
}

// ProvisioningDefaults (cluster-scoped)
export function getProvisioningDefaults(cluster: string): Promise<ProvisioningDefaults> {
  return bffFetch<ProvisioningDefaults>(`/clusters/${enc(cluster)}/provisioningdefaults/varroa-defaults`);
}

export function updateProvisioningDefaults(
  cluster: string,
  config: ProvisioningDefaults
): Promise<ProvisioningDefaults> {
  return bffFetch<ProvisioningDefaults>(`/clusters/${enc(cluster)}/provisioningdefaults/varroa-defaults`, {
    method: "PUT",
    body: JSON.stringify(config),
  });
}

export function getProvisioningConfig(cluster: string): Promise<ProvisioningConfig> {
  return bffFetch<ProvisioningConfig>(`/clusters/${enc(cluster)}/provisioning/config`);
}

export function getVersionProfiles(cluster: string): Promise<VersionProfileDetail[]> {
  return bffFetch<{items: VersionProfileDetail[]}>(`/clusters/${enc(cluster)}/version-profiles`).then(r => r.items);
}

// Version candidates are clusterless (a ProfileCandidate targets a
// cluster-scoped JenkinsVersionProfile, not a per-cluster route segment) —
// matching the /updatecenter no-cluster-segment precedent below.
export function listVersionCandidates(): Promise<VersionCandidateList> {
  return bffFetch<VersionCandidateList>("/version-candidates");
}

export function promoteVersionCandidate(name: string): Promise<VersionCandidate> {
  return bffFetch<VersionCandidate>(`/version-candidates/${enc(name)}/promote`, { method: "POST" });
}

export function listControllerClasses(cluster: string): Promise<ControllerClass[]> {
  return bffFetch<{items: ControllerClass[]}>(`/clusters/${enc(cluster)}/controller-classes`).then(r => r.items);
}

export function getControllerClass(cluster: string, name: string): Promise<ControllerClass> {
  return bffFetch<ControllerClass>(`/clusters/${enc(cluster)}/controller-classes/${encodeURIComponent(name)}`);
}

export function createVersionProfile(cluster: string, profile: Record<string, unknown>): Promise<Record<string, unknown>> {
  return bffFetch<Record<string, unknown>>(`/clusters/${enc(cluster)}/version-profiles`, {
    method: "POST",
    body: JSON.stringify(profile),
  });
}

export function updateVersionProfile(cluster: string, name: string, profile: Record<string, unknown>): Promise<Record<string, unknown>> {
  return bffFetch<Record<string, unknown>>(`/clusters/${enc(cluster)}/version-profiles/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(profile),
  });
}

export function deleteVersionProfile(cluster: string, name: string): Promise<void> {
  return bffFetch<void>(`/clusters/${enc(cluster)}/version-profiles/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export function parsePreflightChecks(err: unknown): PreflightCheck[] | null {
  if (!err || typeof err !== "object" || !("body" in err) || !err.body || typeof err.body !== "object") return null;
  const checks = "checks" in err.body ? err.body.checks : undefined;
  return Array.isArray(checks) ? checks as PreflightCheck[] : null;
}

// ---- API Keys ----

export function createApiKey(expiresIn?: string, name?: string): Promise<{ token: string; warning: string }> {
  const body: Record<string, string> = {};
  if (expiresIn) body.expiresIn = expiresIn;
  if (name) body.name = name;
  return bffFetch("/me/apikeys", { method: "POST", body: JSON.stringify(body) });
}

export function listApiKeys(): Promise<{ items: KeyMeta[] }> {
  return bffFetch<{ items: KeyMeta[] }>("/me/apikeys");
}

export function revokeApiKey(prefix: string): Promise<void> {
  return bffFetch(`/me/apikeys/${encodeURIComponent(prefix)}`, { method: "DELETE" });
}

export function rotateApiKey(
  prefix: string,
  expiresIn?: string,
  name?: string
): Promise<{ token: string; warning: string }> {
  const body: Record<string, string> = {};
  if (expiresIn) body.expiresIn = expiresIn;
  if (name) body.name = name;
  return bffFetch(`/me/apikeys/${encodeURIComponent(prefix)}/rotate`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function adminListUserApiKeys(user: string): Promise<{ items: KeyMeta[] }> {
  return bffFetch<{ items: KeyMeta[] }>(`/users/${encodeURIComponent(user)}/apikeys`);
}

export function adminRevokeApiKey(user: string, prefix: string): Promise<void> {
  return bffFetch(
    `/users/${encodeURIComponent(user)}/apikeys/${encodeURIComponent(prefix)}`,
    { method: "DELETE" }
  );
}

// --- Self-service ---

export interface DeployableNamespaces {
  namespaces: string[];
  defaultNamespace: string;
  allowFreeform: boolean;
  degraded: boolean;
}

export function getDeployableNamespaces(cluster: string): Promise<DeployableNamespaces> {
  return bffFetch<DeployableNamespaces>(
    `/clusters/${encodeURIComponent(cluster)}/namespaces/deployable`
  );
}

export interface ChangePasswordBody {
  oldPassword: string;
  newPassword: string;
}

export function changePassword(body: ChangePasswordBody): Promise<void> {
  return bffFetch("/me/password", {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

// --- Users (admin) ---

export interface UserEntry {
  name: string;
  email?: string;
  displayName?: string;
  groups: string[];
  lastLogin?: string;
  managedBy: string;
  /** Purpose of a local account beside OIDC. */
  purpose?: AccountPurpose;
}

/** The purpose a local account beside OIDC carries. */
export type AccountPurpose = "service" | "break-glass";

export function listUsers(): Promise<UserEntry[]> {
  return bffFetch<{items: UserEntry[]}>("/users").then(r => r.items);
}

export interface CreateUserBody {
  username: string;
  email?: string;
  displayName?: string;
  password: string;
  /** Required when local accounts run beside OIDC. */
  purpose?: AccountPurpose;
}

export function createUser(body: CreateUserBody): Promise<{ name: string }> {
  return bffFetch("/users", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function deleteUser(name: string): Promise<void> {
  return bffFetch(`/users/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export interface UpdateUserBody {
  email?: string;
  displayName?: string;
  /** Required when local accounts run beside OIDC. */
  purpose?: AccountPurpose;
}

export function updateUser(name: string, body: UpdateUserBody): Promise<{ name: string }> {
  return bffFetch(`/users/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export function adminResetPassword(name: string, password: string): Promise<void> {
  return bffFetch(`/users/${encodeURIComponent(name)}/password`, {
    method: "PUT",
    body: JSON.stringify({ newPassword: password }),
  });
}

// --- Groups (admin) ---

export interface GroupEntry {
  name: string;
  displayName?: string;
  members: string[];
  memberCount?: number;
  source?: string;
}

export function listGroups(): Promise<GroupEntry[]> {
  return bffFetch<{items: GroupEntry[]}>("/groups").then(r => r.items);
}

export interface CreateGroupBody {
  name: string;
  displayName?: string;
  members?: string[];
}

export function createGroup(body: CreateGroupBody): Promise<{ name: string }> {
  return bffFetch("/groups", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function updateGroup(name: string, body: { displayName?: string; members: string[] }): Promise<{ name: string }> {
  return bffFetch(`/groups/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export function deleteGroup(name: string): Promise<void> {
  return bffFetch(`/groups/${encodeURIComponent(name)}`, { method: "DELETE" });
}

// --- Identity settings (admin, read-only) ---

export interface IdentitySettings {
  mode: string;
  cookieDomain: string;
  defaultRead: boolean;
  issuer?: string;
  clientId?: string;
  scopes?: string[];
}

export function getIdentitySettings(): Promise<IdentitySettings> {
  return bffFetch("/identity-settings");
}

// --- Built-in roles (admin, read-only) ---

export interface APIRule {
  resources: string[];
  verbs: string[];
}

export interface BuiltinRole {
  name: string;
  apiRules: APIRule[];
  jenkinsRoleRef: string;
  jenkinsPermissions: string[];
}

export function getBuiltinRoles(): Promise<BuiltinRole[]> {
  return bffFetch<{items: BuiltinRole[]}>("/builtin-roles").then(r => r.items);
}

// ---- Configuration Pipeline ----

/** The Varroa page that a Jenkins item path under a headless controller names. */
export interface JenkinsLink {
  link: string;
}

/**
 * Resolves a Jenkins item path (the part after the headless door prefix) to
 * its Varroa page. The path is encoded once here, so a %252F stays %252F on
 * the wire.
 */
export function getControllerJenkinsLink(cluster: string, ns: string, name: string, path: string): Promise<JenkinsLink> {
  return bffFetch<JenkinsLink>(`${ctrlBase(cluster, ns, name)}/jenkins-link?path=${encodeURIComponent(path)}`);
}

export function getControllerDiff(cluster: string, ns: string, name: string): Promise<ControllerDiff> {
  return bffFetch<ControllerDiff>(
    `${ctrlBase(cluster, ns, name)}/diff`
  );
}

// previewControllerOverlay computes the server-side merge of podOverrides +
// resourceOverlay against the chosen baseline (defaults to "live"). Returns the
// merged YAML, per-resource unified diffs, and warn-but-allow guardrail warnings.
// 400 => malformed overlay (body carries `{"error":"<resource>: <msg>"}`),
// 403 => caller lacks controllers:update.
export function previewControllerOverlay(
  cluster: string,
  ns: string,
  name: string,
  body: PreviewRequest,
): Promise<PreviewResponse> {
  return bffFetch<PreviewResponse>(
    `${ctrlBase(cluster, ns, name)}/preview`,
    { method: "POST", body: JSON.stringify(body) }
  );
}

export function pauseBundleRollout(cluster: string, ns: string, name: string): Promise<{ status: string }> {
  return bffFetch<{ status: string }>(
    `/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/pause`,
    { method: "POST" }
  );
}

export function resumeBundleRollout(cluster: string, ns: string, name: string): Promise<{ status: string }> {
  return bffFetch<{ status: string }>(
    `/clusters/${enc(cluster)}/composedbundles/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/resume`,
    { method: "POST" }
  );
}

// --- Teams (admin) ---

export interface TeamNamespaceState {
  name: string;
  state: string;
}

export interface TeamCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export interface TeamEntry {
  name: string;
  displayName?: string;
  members?: string[];
  subjects?: SubjectRef[];
  namespaces: string[];
  roleRef?: string;
  provisionNamespaces?: boolean;
  observedGeneration?: number;
  groupRef?: string;
  bindingRef?: string;
  namespaceStates?: TeamNamespaceState[];
  conditions?: TeamCondition[];
  pipelines?: TeamPipelines;
  placement?: TeamPlacement;
  rehydration?: TeamRehydration;
}

export interface CreateTeamBody {
  name: string;
  displayName?: string;
  members?: string[];
  subjects?: SubjectRef[];
  namespaces: string[];
  roleRef?: string;
  provisionNamespaces?: boolean;
  pipelines?: TeamPipelines;
}

export function listTeams(): Promise<TeamEntry[]> {
  return bffFetch<{items: TeamEntry[]}>("/teams").then(r => r.items);
}

export function getTeam(name: string): Promise<TeamEntry> {
  return bffFetch(`/teams/${encodeURIComponent(name)}`);
}

export function createTeam(body: CreateTeamBody): Promise<{ name: string }> {
  return bffFetch("/teams", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function updateTeam(name: string, body: CreateTeamBody): Promise<{ name: string }> {
  return bffFetch(`/teams/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export function deleteTeam(name: string): Promise<void> {
  return bffFetch(`/teams/${encodeURIComponent(name)}`, { method: "DELETE" });
}

// --- Runners (external agents; design D1, D12) ---

/**
 * Lists Runner objects, narrowed to one team when given. status.token is
 * present only for a caller holding runners:create; the dashboard never
 * reads it.
 */
export function listRunners(team?: string): Promise<RunnerEntry[]> {
  const qs = team ? `?team=${enc(team)}` : "";
  return bffFetch<{ items: RunnerEntry[] }>(`/runners${qs}`).then((r) => r.items);
}

// ---- Brood operations (cross-cluster logical-run DTOs) ----

export function listBroodOperations(namespace?: string, cluster?: string, startedBy?: string): Promise<BroodListResponse> {
  const params = new URLSearchParams();
  if (namespace) params.set("namespace", namespace);
  if (cluster) params.set("cluster", cluster);
  if (startedBy) params.set("startedBy", startedBy);
  const qs = params.toString() ? `?${params.toString()}` : "";
  return bffFetch<BroodListResponse>(`/brood-operations${qs}`);
}

export function getBroodOperation(name: string, namespace: string): Promise<BroodRun> {
  return bffFetch<BroodRun>(
    `/brood-operations/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`
  );
}

export function createBroodOperation(body: CreateBroodRequest): Promise<BroodRun> {
  return bffFetch<BroodRun>("/brood-operations", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function deleteBroodOperation(name: string, namespace: string): Promise<BroodActionResponse> {
  return bffFetch<BroodActionResponse>(
    `/brood-operations/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`,
    { method: "DELETE" }
  );
}

export function suspendBroodOperation(name: string, namespace: string, suspend: boolean): Promise<BroodActionResponse> {
  return bffFetch<BroodActionResponse>(
    `/brood-operations/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/suspend`,
    {
      method: "POST",
      body: JSON.stringify({ suspend }),
    }
  );
}

export function previewBroodOperation(body: CreateBroodRequest): Promise<BroodPreviewResponse> {
  return bffFetch<BroodPreviewResponse>("/brood-operations/preview", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function broodStreamUrl(name: string, namespace: string): string {
  return `${BFF_BASE}/brood-operations/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/stream`;
}

/** teamRunStreamUrl builds the team run stream URL on BFF_BASE, so the
 * browser reaches the API and not the SPA. useEventStream appends the
 * stream ticket as a query parameter; this URL holds none. */
export function teamRunStreamUrl(team: string): string {
  return `${BFF_BASE}/pipelines/${encodeURIComponent(team)}/stream`;
}

// ---- Brood Schedules ----

export function listBroodSchedules(namespace?: string): Promise<BroodScheduleListResponse> {
  const params = new URLSearchParams();
  if (namespace) params.set("namespace", namespace);
  const qs = params.toString() ? `?${params.toString()}` : "";
  return bffFetch<BroodScheduleListResponse>(`/brood-schedules${qs}`);
}

export function getBroodSchedule(namespace: string, name: string): Promise<BroodSchedule> {
  return bffFetch<BroodSchedule>(
    `/brood-schedules/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`
  );
}

export function createBroodSchedule(body: CreateBroodScheduleRequest): Promise<BroodSchedule> {
  return bffFetch<BroodSchedule>("/brood-schedules", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function deleteBroodSchedule(namespace: string, name: string): Promise<unknown> {
  return bffFetch<unknown>(
    `/brood-schedules/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`,
    { method: "DELETE" }
  );
}

export function suspendBroodSchedule(namespace: string, name: string, suspend: boolean): Promise<unknown> {
  return bffFetch<unknown>(
    `/brood-schedules/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/suspend`,
    {
      method: "POST",
      body: JSON.stringify({ suspend }),
    }
  );
}

// ---- Update Center ----

export function getUpdateCenterStatus(): Promise<UpdateCenterStatus> {
  return bffFetch<UpdateCenterStatus>("/updatecenter");
}

export function getUpdateCenterPlugins(q?: string): Promise<UpdateCenterPlugins> {
  const qs = q ? `?q=${encodeURIComponent(q)}` : "";
  return bffFetch<UpdateCenterPlugins>(`/updatecenter/plugins${qs}`);
}

/**
 * Uploads a plugin artifact. With dryRun the closure is resolved and validated
 * and nothing is stored, which is cheap enough server-side to be the default
 * path rather than an advanced option.
 */
export function uploadUpdateCenterPlugin(file: File, dryRun: boolean): Promise<UpdateCenterUploadResult> {
  const body = new FormData();
  body.append("file", file);
  return bffUpload<UpdateCenterUploadResult>(`/updatecenter/plugins${dryRun ? "?dryRun=true" : ""}`, body);
}

// ---- Fleet Plugin Inventory ----

export interface FleetPluginsParams {
  q?: string;
  cluster?: string;
  namespace?: string;
  affected?: string;
}

function fleetPluginsQS(params?: FleetPluginsParams): string {
  if (!params) return "";
  const parts: string[] = [];
  if (params.q) parts.push(`q=${encodeURIComponent(params.q)}`);
  if (params.cluster) parts.push(`cluster=${encodeURIComponent(params.cluster)}`);
  if (params.namespace) parts.push(`namespace=${encodeURIComponent(params.namespace)}`);
  if (params.affected) parts.push(`affected=${encodeURIComponent(params.affected)}`);
  return parts.length > 0 ? `?${parts.join("&")}` : "";
}

export function listFleetPlugins(params?: FleetPluginsParams): Promise<FleetPluginsRollup> {
  return bffFetch<FleetPluginsRollup>(`/fleet/plugins${fleetPluginsQS(params)}`);
}

export function getFleetPlugin(name: string, params?: FleetPluginsParams): Promise<FleetPluginDrilldown> {
  return bffFetch<FleetPluginDrilldown>(`/fleet/plugins/${encodeURIComponent(name)}${fleetPluginsQS(params)}`);
}

// ---- Pipelines ----
//
// The BFF answers a run request, a cancel, and a rerun with 202 and a JSON
// body the caller needs (runId, intentKey, state) — unlike this file's other
// 202 actions, whose body carries nothing the caller reads. bffFetch treats
// every 202 as empty, so these three calls go through bffFetchText and parse
// the body themselves instead.
async function bffFetchJSON<T>(path: string, options?: RequestInit): Promise<T> {
  const text = await bffFetchText(path, options);
  return (text ? JSON.parse(text) : undefined) as T;
}

/**
 * Fetches a binary response with the bearer token attached, mirroring
 * useApi's request() but returning a Blob instead of decoding JSON. Kept
 * local to the client: it is the pipelines client's one binary download.
 */
async function bffFetchBlob(path: string): Promise<Blob> {
  const token = getToken();
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = `Bearer ${token}`;
  let res: Response;
  try {
    res = await fetch(`${BFF_BASE}${path}`, { headers });
  } catch {
    throw new ApiError(0, "Varroa could not reach the server.");
  }
  if (!res.ok) {
    throw new ApiError(res.status, "Varroa could not download this file.");
  }
  return res.blob();
}

const pipelinesBase = (team: string) => `/pipelines/${enc(team)}`;
// {job} is one path segment holding the percent-encoded job path (D2): a job
// path payments/checkout-api/main becomes payments%2Fcheckout-api%2Fmain.
const jobBase = (team: string, jobPath: string) => `${pipelinesBase(team)}/jobs/${enc(jobPath)}`;
const runBase = (runId: string) => `/runs/${enc(runId)}`;

/**
 * Lists every pipelines Team the caller may read, on this cluster and on
 * each member cluster. The pipelines landing page reads this route.
 */
export function listPipelineTeams(): Promise<PipelineTeamList> {
  return bffFetch<PipelineTeamList>("/pipelines");
}

export function listPipelines(team: string): Promise<PipelineListResponse> {
  return bffFetch<PipelineListResponse>(pipelinesBase(team));
}

export function getPipelineJob(team: string, jobPath: string): Promise<PipelineJobDetail> {
  return bffFetch<PipelineJobDetail>(jobBase(team, jobPath));
}

/** listJobBranches reads the branch and change-request inventory of one
 * multibranch job. The inventory can be up to ten minutes stale. */
export function listJobBranches(team: string, jobPath: string): Promise<JobBranchList> {
  return bffFetch<JobBranchList>(`${jobBase(team, jobPath)}/branches`);
}

/** scanMultibranch requests a scan of a multibranch job's branches and pull
 * requests. The 202 answer carries the scan identifier; the inventory's
 * lastScan.scanId repeats it once the scan reports. */
export function scanMultibranch(team: string, jobPath: string, cluster?: string): Promise<ScanRequestedResponse> {
  return bffFetchJSON<ScanRequestedResponse>(`${jobBase(team, jobPath)}/scan`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(cluster ? { cluster } : {}),
  });
}

/**
 * Reads the team's own view of the cell that hosts its pipelines tenant.
 * A team that waits for its first cell answers state "unplaced" rather
 * than a 404.
 */
export function getTeamCell(team: string): Promise<TeamCell> {
  return bffFetch<TeamCell>(`${pipelinesBase(team)}/cell`);
}

/**
 * Lists the fleet locks a team's own tenants own or wait on. A team that
 * spans two tenants answers both tenants' rows in one list.
 */
export function listFleetLocks(team: string): Promise<FleetLockListResponse> {
  return bffFetch<FleetLockListResponse>(`${pipelinesBase(team)}/locks`);
}

/**
 * Lists every tenant's fleet locks. The operator page reads this route,
 * and the read covers every cluster.
 */
export function listAllFleetLocks(): Promise<FleetLockListResponse> {
  return bffFetch<FleetLockListResponse>("/locks");
}

/**
 * Force-releases one releasing fleet lock. The caller needs the
 * cluster-wide fleetlocks:release verb. The server answers 409 for a lock
 * that is not releasing or whose holder pod still runs, and 404 for an
 * unknown lock. The second 409 carries holderCell in its body (a
 * FleetLockReleaseConflict), which the ApiError body holds.
 */
export function releaseFleetLock(body: FleetLockReleaseRequest): Promise<FleetLockReleaseResponse> {
  return bffFetch<FleetLockReleaseResponse>("/locks/release", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

/**
 * Lists the load of every cell the caller may read in one cluster. The
 * cluster defaults to the local cluster; namespace narrows the list.
 */
export function listCellUsage(params?: { cluster?: string; namespace?: string }): Promise<CellUsageListResponse> {
  const qs = new URLSearchParams();
  if (params?.cluster) qs.set("cluster", params.cluster);
  if (params?.namespace) qs.set("namespace", params.namespace);
  const s = qs.toString();
  return bffFetch<CellUsageListResponse>(`/cells/usage${s ? `?${s}` : ""}`);
}

/**
 * Reads one cell's load, tenant by tenant. An unknown controller, a
 * controller that is not a cell, and a refused caller all answer 404.
 */
export function getCellUsage(namespace: string, name: string, cluster?: string): Promise<CellUsage> {
  const qs = cluster ? `?cluster=${encodeURIComponent(cluster)}` : "";
  return bffFetch<CellUsage>(`/cells/${enc(namespace)}/${enc(name)}/usage${qs}`);
}

export interface ListRunsParams {
  branch?: string;
  result?: string;
  actor?: string;
  cause?: string;
  limit?: number;
  cursor?: string;
}

function listRunsQS(params?: ListRunsParams): string {
  if (!params) return "";
  const qs = new URLSearchParams();
  if (params.branch) qs.set("branch", params.branch);
  if (params.result) qs.set("result", params.result);
  if (params.actor) qs.set("actor", params.actor);
  if (params.cause) qs.set("cause", params.cause);
  if (params.limit != null) qs.set("limit", String(params.limit));
  if (params.cursor) qs.set("cursor", params.cursor);
  const s = qs.toString();
  return s ? `?${s}` : "";
}

export function listRuns(team: string, jobPath: string, params?: ListRunsParams): Promise<RunListResponse> {
  return bffFetch<RunListResponse>(`${jobBase(team, jobPath)}/runs${listRunsQS(params)}`);
}

/**
 * Requests a new build. Fire and forget: the BFF does not claim the build
 * number, so the 202 body's runId is the caller's only handle on the run
 * before it polls getRun.
 */
export function createRun(team: string, jobPath: string, body?: CreateRunRequest): Promise<RunActionResponse> {
  return bffFetchJSON<RunActionResponse>(`${jobBase(team, jobPath)}/runs`, {
    method: "POST",
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
}

export function getRun(runId: string): Promise<RunDetail> {
  return bffFetch<RunDetail>(runBase(runId));
}

export interface RunLogUrlParams {
  /** Byte offset of the first unread byte. */
  from?: number;
  /** 1-based line number starting at from. */
  fromLine?: number;
  /** true returns text/plain with no SSE. */
  raw?: boolean;
}

/**
 * Builds the run log stream URL. Pass it as the baseUrl to useEventStream
 * for the SSE mode (scope `run:<runId>`, event names "meta", "stage",
 * "line", "gap", "live", "end"), or with raw:true for the plain-text link
 * that opens in a new tab.
 */
export function runLogUrl(runId: string, params?: RunLogUrlParams): string {
  const qs = new URLSearchParams();
  if (params?.from != null) qs.set("from", String(params.from));
  if (params?.fromLine != null) qs.set("fromLine", String(params.fromLine));
  if (params?.raw) qs.set("raw", "true");
  const s = qs.toString();
  return `${BFF_BASE}${runBase(runId)}/log${s ? `?${s}` : ""}`;
}

export function listRunArtifacts(runId: string): Promise<RunArtifactListResponse> {
  return bffFetch<RunArtifactListResponse>(`${runBase(runId)}/artifacts`);
}

/**
 * Downloads one artifact's bytes. path is the multi-segment artifact path
 * exactly as run_artifacts.path stored it; each segment is percent-encoded
 * on its own so the slashes that separate segments survive.
 */
export function downloadRunArtifact(runId: string, path: string): Promise<Blob> {
  const encodedPath = path.split("/").map(enc).join("/");
  return bffFetchBlob(`${runBase(runId)}/artifacts/${encodedPath}`);
}

export function listRunTests(runId: string): Promise<RunTestListResponse> {
  return bffFetch<RunTestListResponse>(`${runBase(runId)}/tests`);
}

/**
 * Reads the recent outcomes of one test case of one job, newest first.
 * caseKey is the opaque key the tests read returned. The key can hold a
 * "/" and a "%2F", so it is encoded once more as one path segment. The
 * client never parses the key.
 */
export function getTestCaseHistory(
  team: string,
  jobPath: string,
  caseKey: string,
  limit?: number,
): Promise<TestCaseHistoryResponse> {
  const qs = limit != null ? `?limit=${limit}` : "";
  return bffFetch<TestCaseHistoryResponse>(`${jobBase(team, jobPath)}/tests/${enc(caseKey)}${qs}`);
}

export function listRunChanges(runId: string): Promise<RunChangeListResponse> {
  return bffFetch<RunChangeListResponse>(`${runBase(runId)}/changes`);
}

/** One page of the runs a run started. Pass the previous page's nextCursor
 * to read the next page. */
export function listRunDownstream(runId: string, cursor?: string): Promise<RunLinkPage> {
  const qs = cursor ? `?cursor=${enc(cursor)}` : "";
  return bffFetch<RunLinkPage>(`${runBase(runId)}/downstream${qs}`);
}

/** Requests that a queued or running build stop. Fire and forget. */
export function cancelRun(runId: string): Promise<CancelRunResponse> {
  return bffFetchJSON<CancelRunResponse>(`${runBase(runId)}/cancel`, { method: "POST" });
}

/**
 * Requests a retry of a finished run. One retry per source run, fleet-wide:
 * a second rerun still answers 202, reusing the first retry's identifier.
 */
export function rerunRun(runId: string): Promise<RunActionResponse> {
  return bffFetchJSON<RunActionResponse>(`${runBase(runId)}/rerun`, { method: "POST" });
}

/** Reads the script a finished run ran, and whether the run replays. */
export function getRunScript(runId: string): Promise<RunScript> {
  return bffFetch<RunScript>(`${runBase(runId)}/script`);
}

/**
 * Requests a fresh build of the run's job with an edited script. Each call
 * mints a new run identifier, so each call starts its own build.
 */
export function replayRun(runId: string, script: string): Promise<RunActionResponse> {
  return bffFetchJSON<RunActionResponse>(`${runBase(runId)}/replay`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ script }),
  });
}

/** Reads whether a finished run restarts from a stage, and the stages it can restart from. */
export function getRunRestart(runId: string): Promise<RunRestart> {
  return bffFetch<RunRestart>(`${runBase(runId)}/restart`);
}

/**
 * Requests a fresh build of the run's job from one stage. Each call mints a
 * new run identifier, so each call starts its own build.
 */
export function restartRun(runId: string, stage: string): Promise<RunActionResponse> {
  return bffFetchJSON<RunActionResponse>(`${runBase(runId)}/restart`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ stage }),
  });
}

/** Answers a paused input step with parameter values. Sends
 * { "parameters": { name: value } }. Fire and forget: the response
 * carries no outcome. */
export function proceedRunInput(
  runId: string,
  inputId: string,
  parameters: Record<string, string>,
): Promise<CancelRunResponse> {
  return bffFetchJSON<CancelRunResponse>(`${runBase(runId)}/inputs/${enc(inputId)}/proceed`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ parameters }),
  });
}

/** Stops a paused input step. Takes no body and reports no outcome. */
export function abortRunInput(runId: string, inputId: string): Promise<CancelRunResponse> {
  return bffFetchJSON<CancelRunResponse>(`${runBase(runId)}/inputs/${enc(inputId)}/abort`, {
    method: "POST",
  });
}

/** clusterQuery builds the optional cluster query string. */
function clusterQuery(cluster?: string): string {
  return cluster ? `?cluster=${encodeURIComponent(cluster)}` : "";
}

/**
 * Lists the cell pools of every cluster, or of one cluster. The surface
 * needs the cellpools:read verb on a cluster-wide binding, because
 * CellPool is cluster scoped.
 */
export function getCellPools(cluster?: string): Promise<CellPoolListResponse> {
  return bffFetch<CellPoolListResponse>(`/cellpools${clusterQuery(cluster)}`);
}

/**
 * Reads one cell pool's spec and status. With no cluster, the BFF reads
 * its own cluster.
 */
export function getCellPool(name: string, cluster?: string): Promise<CellPool> {
  return bffFetch<CellPool>(`/cellpools/${enc(name)}${clusterQuery(cluster)}`);
}

/**
 * Scales one cell pool. The body carries the four scale dials only: a
 * structural edit goes to kubectl or GitOps. The handler retries an
 * internal conflict three times and then answers 409.
 */
export function patchCellPool(name: string, cluster: string, body: CellPoolScaleRequest): Promise<CellPool> {
  return bffFetch<CellPool>(`/cellpools/${enc(name)}${clusterQuery(cluster)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

/**
 * Reads which subjects may build on each tenant of a cell. The read
 * answers 404 for an unknown controller, a controller that is not a cell,
 * and a refused controller read. It answers 403 for a caller who holds no
 * cluster-wide rolebindings:read.
 */
export function getCellGrants(namespace: string, name: string, cluster?: string): Promise<CellGrants> {
  const qs = cluster ? `?cluster=${encodeURIComponent(cluster)}` : "";
  return bffFetch<CellGrants>(`/cells/${enc(namespace)}/${enc(name)}/grants${qs}`);
}

// ---- Assistant and run notes (add-assistant-ui) ----

/** The request body that starts a session. Send either message or prompt. */
export interface AssistantSessionCreate {
  context?: AssistantSessionContext;
  message?: string;
  prompt?: string;
}

export function createAssistantSession(body: AssistantSessionCreate): Promise<AssistantSession> {
  return bffFetch<AssistantSession>("/assistant/sessions", { method: "POST", body: JSON.stringify(body) });
}

export function listAssistantSessions(
  params: { cursor?: string; limit?: number; prompt?: string; team?: string } = {},
): Promise<AssistantSessionList> {
  const q = new URLSearchParams();
  if (params.cursor) q.set("cursor", params.cursor);
  if (params.limit) q.set("limit", String(params.limit));
  if (params.prompt) q.set("prompt", params.prompt);
  if (params.team) q.set("team", params.team);
  const qs = q.toString();
  return bffFetch<AssistantSessionList>(`/assistant/sessions${qs ? `?${qs}` : ""}`);
}

/** Starts a session that proposes a first Jenkinsfile for a repository of a team. */
export function proposePipeline(team: string, repo: string): Promise<ProposePipelineResponse> {
  return bffFetch<ProposePipelineResponse>(`${pipelinesBase(team)}/propose`, {
    method: "POST",
    body: JSON.stringify({ repo }),
  });
}

/** Reads the accepted Team agent of a pipelines tenant. 404 when none is accepted. */
export function getTeamAgent(team: string): Promise<TeamAgent> {
  return bffFetch<TeamAgent>(`${pipelinesBase(team)}/agent`);
}

export function listTeamAgentSessions(
  team: string,
  params: { state?: string; cursor?: string; limit?: number } = {},
): Promise<TeamAgentSessionList> {
  const q = new URLSearchParams();
  if (params.state) q.set("state", params.state);
  if (params.cursor) q.set("cursor", params.cursor);
  if (params.limit) q.set("limit", String(params.limit));
  const qs = q.toString();
  return bffFetch<TeamAgentSessionList>(`${pipelinesBase(team)}/agent/sessions${qs ? `?${qs}` : ""}`);
}

export function getTeamAgentCapabilities(team: string): Promise<TeamAgentCapabilities> {
  return bffFetch<TeamAgentCapabilities>(`${pipelinesBase(team)}/agent/capabilities`);
}

export function runTeamAgent(team: string, body: { job?: string; run?: string } = {}): Promise<{ state: string }> {
  return bffFetchJSON<{ state: string }>(`${pipelinesBase(team)}/agent/run`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function listAgents(): Promise<TeamAgentItemList> {
  return bffFetch<TeamAgentItemList>("/agents");
}

export function putAgentUnattended(namespace: string, name: string, enabled: boolean): Promise<TeamAgentUnattendedResult> {
  return bffFetchJSON<TeamAgentUnattendedResult>(`/agents/${enc(namespace)}/${enc(name)}/unattended`, {
    method: "PUT",
    body: JSON.stringify({ enabled }),
  });
}

export function getAssistantSession(id: string): Promise<AssistantSessionDetail> {
  return bffFetch<AssistantSessionDetail>(`/assistant/sessions/${enc(id)}`);
}

export function postAssistantMessage(id: string, body: { message: string }): Promise<AssistantAcceptedState> {
  return bffFetchJSON<AssistantAcceptedState>(`/assistant/sessions/${enc(id)}/messages`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function cancelAssistantSession(id: string): Promise<AssistantAcceptedState> {
  return bffFetchJSON<AssistantAcceptedState>(`/assistant/sessions/${enc(id)}/cancel`, { method: "POST" });
}

export function listAssistantApprovals(params: {
  state?: string;
  session?: string;
  targetKind?: string;
  targetRef?: string;
  scope?: string;
  cursor?: string;
  limit?: number;
} = {}): Promise<AssistantApprovalList> {
  const q = new URLSearchParams();
  if (params.state) q.set("state", params.state);
  if (params.session) q.set("session", params.session);
  if (params.targetKind) q.set("target_kind", params.targetKind);
  if (params.targetRef) q.set("target_ref", params.targetRef);
  if (params.scope) q.set("scope", params.scope);
  if (params.cursor) q.set("cursor", params.cursor);
  if (params.limit) q.set("limit", String(params.limit));
  const qs = q.toString();
  return bffFetch<AssistantApprovalList>(`/assistant/approvals${qs ? `?${qs}` : ""}`);
}

export function approveAssistantApproval(id: string): Promise<AssistantApprovalOutcome> {
  return bffFetchJSON<AssistantApprovalOutcome>(`/assistant/approvals/${enc(id)}/approve`, { method: "POST" });
}

export function denyAssistantApproval(id: string, body: { reason?: string } = {}): Promise<AssistantApprovalOutcome> {
  return bffFetchJSON<AssistantApprovalOutcome>(`/assistant/approvals/${enc(id)}/deny`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function getAssistantSettings(): Promise<AssistantSettings> {
  return bffFetch<AssistantSettings>("/assistant/settings");
}

export function putAssistantSettings(body: AssistantSettingsUpdate): Promise<AssistantSettings> {
  return bffFetch<AssistantSettings>("/assistant/settings", { method: "PUT", body: JSON.stringify(body) });
}

export function listAssistantProviders(): Promise<AssistantProviderList> {
  return bffFetch<AssistantProviderList>("/assistant/providers");
}

export function getMyAssistant(): Promise<MyAssistant> {
  return bffFetch<MyAssistant>("/me/assistant");
}

export function putMyAssistant(body: MyAssistantUpdate): Promise<MyAssistant> {
  return bffFetch<MyAssistant>("/me/assistant", { method: "PUT", body: JSON.stringify(body) });
}

export function putMyAssistantKey(body: MyAssistantKeyRequest): Promise<MyAssistant> {
  return bffFetch<MyAssistant>("/me/assistant/key", { method: "PUT", body: JSON.stringify(body) });
}

export function deleteMyAssistantKey(): Promise<void> {
  return bffFetch<void>("/me/assistant/key", { method: "DELETE" });
}

export function listRunNotes(runId: string): Promise<RunNoteList> {
  return bffFetch<RunNoteList>(`/runs/${enc(runId)}/notes`);
}

export function createRunNote(runId: string, body: CreateRunNoteRequest): Promise<RunNote> {
  return bffFetch<RunNote>(`/runs/${enc(runId)}/notes`, { method: "POST", body: JSON.stringify(body) });
}

export function listJobNotes(team: string, jobPath: string): Promise<RunNoteList> {
  return bffFetch<RunNoteList>(`${jobBase(team, jobPath)}/notes`);
}

/** assistantEventsUrl is the stream URL of one session, resumed after seq. */
export function assistantEventsUrl(id: string, after: number): string {
  return `${BFF_BASE}/assistant/sessions/${enc(id)}/events?after=${after}`;
}
