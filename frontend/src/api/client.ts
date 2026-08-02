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
} from "../types";
import type { Permissions } from "../types/auth";
import { ApiError, bffFetch, bffFetchText, bffUpload } from "../hooks/useApi";

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
): Promise<Controller> {
  pruneEmptyProbes(patch);
  const url = opts?.force
    ? `${ctrlBase(cluster, namespace, name)}?force=true`
    : ctrlBase(cluster, namespace, name);
  return bffFetch<Controller>(url, {
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

// Approve restart
export function approveRestart(
  cluster: string,
  ns: string,
  name: string,
  action: "reload" | "restart",
): Promise<{ status: string; action: string }> {
  return bffFetch<{ status: string; action: string }>(
    `${ctrlBase(cluster, ns, name)}/approve`,
    { method: "POST", body: JSON.stringify({ action }) }
  );
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

export const setPowerState = (cluster: string, name: string, ns: string, state: "Running" | "Stopped" | "Hibernated") =>
  updateController(cluster, name, ns, { spec: { powerState: state } });

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
}

export function listUsers(): Promise<UserEntry[]> {
  return bffFetch<{items: UserEntry[]}>("/users").then(r => r.items);
}

export interface CreateUserBody {
  username: string;
  email?: string;
  displayName?: string;
  password: string;
  groups?: string[];
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
}

export interface CreateTeamBody {
  name: string;
  displayName?: string;
  members?: string[];
  subjects?: SubjectRef[];
  namespaces: string[];
  roleRef?: string;
  provisionNamespaces?: boolean;
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
