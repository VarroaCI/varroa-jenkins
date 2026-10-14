import type {
  Controller,
  ControllerSpec,
  ControllerStatus,
  ControllerPhase,
  ComposedBundle,
  ComposedBundleSpec,
  ComposedBundleStatus,
  CatalogItem,
  CatalogItemSummary,
  CatalogItemSpec,
  CatalogSource,
  CatalogSourceSpec,
  VarroaRole,
  VarroaRoleSpec,
  VarroaRoleBinding,
  VarroaRoleBindingSpec,
  JenkinsRole,
  JenkinsRoleSpec,
  JenkinsRoleBinding,
  JenkinsRoleBindingSpec,
  ProvisioningDefaults,
  ProvisioningDefaultsSpec,
  ActivityEvent,
  ObjectMeta,
  ComposedBundlePreview,
  ControllerCondition,
  PipelineSummary,
  RunningRun,
  FleetLock,
} from "../types";
import type { Permissions, MeResponse, AuthConfig } from "../types/auth";
import type { ControllerListItem, ControllerDetail } from "../hooks/useControllers";

// ---- Helpers ----

let idCounter = 0;
function nextId(prefix = "test"): string {
  return `${prefix}-${++idCounter}`;
}

function meta(overrides?: Partial<ObjectMeta>): ObjectMeta {
  return {
    name: overrides?.name ?? nextId("test"),
    namespace: overrides?.namespace ?? "default",
    creationTimestamp: new Date().toISOString(),
    ...overrides,
  };
}

function conditions(): ControllerCondition[] {
  return [{ type: "Ready", status: "True", lastTransitionTime: new Date().toISOString() }];
}

const DEFAULT_PHASE: ControllerPhase = "Running";

// ---- Controller ----

export function createControllerSpec(overrides?: Partial<ControllerSpec>): ControllerSpec {
  return {
    version: "2.492.3",
    composedBundleRef: { name: "base-bundle" },
    pluginSpec: {
      policy: "inherit",
      entries: [{ artifactId: "git", version: "5.0.0" }],
    },
    reconciliationPolicy: { mode: "automatic", interval: "30s" },
    ...overrides,
  };
}

export function createControllerStatus(overrides?: Partial<ControllerStatus>): ControllerStatus {
  return {
    phase: DEFAULT_PHASE,
    endpoint: "https://jenkins.example.com",
    conditions: conditions(),
    desiredStateHash: "abc123",
    ...overrides,
  };
}

export function createController(overrides?: Partial<Controller>): Controller {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "Controller",
    metadata: m,
    spec: createControllerSpec(overrides?.spec),
    status: createControllerStatus(overrides?.status),
  };
}

export function createControllerListItem(
  overrides?: Partial<ControllerListItem>,
): ControllerListItem {
  return {
    cluster: "core",
    name: nextId("ctrl"),
    namespace: "default",
    phase: "Running",
    endpoint: "https://jenkins.example.com",
    miteConnected: true,
    composedBundleRef: { name: "base-bundle" },
    kind: "stateful",
    createdAt: "2026-01-02T03:04:05Z",
    ...overrides,
  };
}

// createControllerFleet builds a deterministic fleet of n controllers for
// scale tests. The phase mix repeats every 20 rows so every bucket is present.
export function createControllerFleet(n: number): ControllerListItem[] {
  return Array.from({ length: n }, (_, i) => {
    const isCell = i % 10 === 5;
    const base = {
      name: `fleet-${String(i).padStart(4, "0")}`,
      namespace: `ns-${i % 20}`,
      cluster: i % 10 === 0 ? "member-1" : "core",
      kind: (isCell ? "cell" : "stateful") as ControllerListItem["kind"],
      jenkinsVersion: "2.504.3",
      version: "2.504.3",
      jenkinsHealth: "healthy",
      lastSeen: "2026-10-08T12:00:00Z",
    };
    const cell = isCell ? { cellPool: "default", tenantCount: 2, maxTenants: 8, warm: false } : {};
    const phase = i % 20;
    if (phase === 0) {
      return createControllerListItem({ ...base, ...cell, phase: "Failed", miteConnected: false, attention: { kind: "bootFailed", message: "exit 5" } });
    }
    if (phase === 1) return createControllerListItem({ ...base, ...cell, phase: "Provisioning", miteConnected: false });
    if (phase === 2) return createControllerListItem({ ...base, ...cell, phase: "Hibernated", miteConnected: false });
    if (phase === 3) return createControllerListItem({ ...base, ...cell, phase: "Running", miteConnected: false });
    return createControllerListItem({ ...base, ...cell, phase: "Connected", miteConnected: true });
  });
}

export function createControllerDetail(
  overrides?: Partial<ControllerDetail>,
): ControllerDetail {
  return {
    cluster: "core",
    name: nextId("ctrl"),
    namespace: "default",
    phase: "Running",
    endpoint: "https://jenkins.example.com",
    version: "2.492.3",
    miteConnected: true,
    reconciliationPolicy: { mode: "automatic", interval: "30s" },
    composedBundleRef: { name: "base-bundle" },
    // The detail contract carries the full spec (spec editor baseline).
    spec: createControllerSpec(),
    // reconcileBlocked is always present on the detail contract (never nil
    // server-side); default to the not-blocked shape.
    reconcileBlocked: { blocked: false },
    // kind is always present on the detail contract; the Cell tab reads it.
    kind: "stateful",
    ...overrides,
  };
}

// ---- ComposedBundle ----

export function createComposedBundleSpec(
  overrides?: Partial<ComposedBundleSpec>,
): ComposedBundleSpec {
  return {
    displayName: "Test Bundle",
    description: "A test composed bundle",
    inputs: [{ itemRef: { name: "test-item" } }],
    variables: {},
    ...overrides,
  };
}

export function createComposedBundleStatus(
  overrides?: Partial<ComposedBundleStatus>,
): ComposedBundleStatus {
  return {
    phase: "Ready",
    itemCount: 1,
    ...overrides,
  };
}

export function createComposedBundle(
  overrides?: Partial<ComposedBundle>,
): ComposedBundle {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "ComposedBundle",
    metadata: m,
    spec: createComposedBundleSpec(overrides?.spec),
    status: createComposedBundleStatus(overrides?.status),
  };
}

export function createComposedBundlePreview(
  overrides?: Partial<ComposedBundlePreview>,
): ComposedBundlePreview {
  return {
    bundleYaml: "bundle: test\n",
    jenkinsYaml: "jenkins: test\n",
    pluginsYaml: "plugins: []\n",
    itemsYaml: "items: []\n",
    rbacYaml: "rbac: []\n",
    missing: [],
    drifted: [],
    warnings: [],
    ...overrides,
  };
}

// ---- CatalogItem ----

export function createCatalogItemSpec(
  overrides?: Partial<CatalogItemSpec>,
): CatalogItemSpec {
  return {
    sourceRef: "test-source",
    type: "jcasc",
    path: "test/casc.yaml",
    displayName: "Test Catalog Item",
    description: "A test catalog item",
    ...overrides,
  };
}

export function createCatalogItem(overrides?: Partial<CatalogItem>): CatalogItem {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "CatalogItem",
    metadata: m,
    spec: createCatalogItemSpec(overrides?.spec),
    status: { valid: true },
    ...overrides,
  };
}

export function createCatalogItemSummary(overrides?: Partial<CatalogItemSummary>): CatalogItemSummary {
  return {
    name: overrides?.name ?? nextId("item"),
    namespace: overrides?.namespace ?? "default",
    displayName: overrides?.displayName ?? "Test Catalog Item",
    type: overrides?.type ?? "jcasc",
    sourceRef: overrides?.sourceRef ?? "test-source",
    version: overrides?.version,
    description: overrides?.description ?? "A test catalog item",
    tags: overrides?.tags,
    valid: overrides?.valid ?? true,
    message: overrides?.message,
    contentHash: overrides?.contentHash,
  };
}

// ---- CatalogSource ----

export function createCatalogSourceSpec(
  overrides?: Partial<CatalogSourceSpec>,
): CatalogSourceSpec {
  return {
    repoURL: "https://github.com/example/catalog.git",
    revision: "main",
    path: "/",
    syncIntervalSeconds: 300,
    trusted: false,
    ...overrides,
  };
}

export function createCatalogSource(
  overrides?: Partial<CatalogSource>,
): CatalogSource {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "CatalogSource",
    metadata: m,
    spec: createCatalogSourceSpec(overrides?.spec),
    status: { phase: "Ready", itemCount: 5 },
    ...overrides,
  };
}

// ---- VarroaRole ----

export function createVarroaRoleSpec(
  overrides?: Partial<VarroaRoleSpec>,
): VarroaRoleSpec {
  return {
    apiRules: [{ resources: ["controllers"], verbs: ["get", "list"] }],
    ...overrides,
  };
}

export function createRole(overrides?: Partial<VarroaRole>): VarroaRole {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "VarroaRole",
    metadata: m,
    spec: createVarroaRoleSpec(overrides?.spec),
    ...overrides,
  };
}

// ---- VarroaRoleBinding ----

export function createVarroaRoleBindingSpec(
  overrides?: Partial<VarroaRoleBindingSpec>,
): VarroaRoleBindingSpec {
  return {
    subjects: [{ kind: "User", name: "test-user" }],
    roleRef: "test-role",
    ...overrides,
  };
}

export function createRoleBinding(
  overrides?: Partial<VarroaRoleBinding>,
): VarroaRoleBinding {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "VarroaRoleBinding",
    metadata: m,
    spec: createVarroaRoleBindingSpec(overrides?.spec),
    ...overrides,
  };
}

// ---- JenkinsRole ----

export function createJenkinsRoleSpec(
  overrides?: Partial<JenkinsRoleSpec>,
): JenkinsRoleSpec {
  return {
    roleType: "Global",
    permissions: ["hudson.model.Hudson.Read"],
    description: "Test Jenkins role",
    ...overrides,
  };
}

export function createJenkinsRole(
  overrides?: Partial<JenkinsRole>,
): JenkinsRole {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "JenkinsRole",
    metadata: m,
    spec: createJenkinsRoleSpec(overrides?.spec),
    ...overrides,
  };
}

// ---- JenkinsRoleBinding ----

export function createJenkinsRoleBindingSpec(
  overrides?: Partial<JenkinsRoleBindingSpec>,
): JenkinsRoleBindingSpec {
  return {
    subjects: [{ kind: "User", name: "test-user" }],
    roleRef: "test-jenkins-role",
    ...overrides,
  };
}

export function createJenkinsRoleBinding(
  overrides?: Partial<JenkinsRoleBinding>,
): JenkinsRoleBinding {
  const m = meta(overrides?.metadata);
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "JenkinsRoleBinding",
    metadata: m,
    spec: createJenkinsRoleBindingSpec(overrides?.spec),
    ...overrides,
  };
}

// ---- ProvisioningDefaults ----

export function createProvisioningConfig(
  overrides?: Partial<ProvisioningDefaults>,
): ProvisioningDefaults {
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "ProvisioningDefaults",
    metadata: { name: "varroa-defaults" },
    spec: createProvisioningDefaultsSpec(overrides?.spec),
    ...overrides,
  };
}

export function createProvisioningDefaultsSpec(
  overrides?: Partial<ProvisioningDefaultsSpec>,
): ProvisioningDefaultsSpec {
  return {
    rootDomain: "example.com",
    defaultCPU: "1",
    defaultMemory: "2Gi",
    defaultStorage: "10Gi",
    defaultPlugins: [{ artifactId: "git", version: "5.0.0" }],
    ...overrides,
  };
}

// ---- Permissions & Auth ----

export function createPermissions(
  global?: Record<string, Record<string, boolean>>,
  scopes?: Array<{
    namespaces: string[];
    hasControllerSelector: boolean;
    capabilities: Record<string, Record<string, boolean>>;
  }>,
): Permissions {
  return {
    global: global ?? {
      controllers: { get: true, list: true, create: true, update: true, delete: true },
      roles: { get: true, list: true, create: true, update: true, delete: true },
      rolebindings: { get: true, list: true, create: true, update: true, delete: true },
      catalogsources: { get: true, list: true, create: true, update: true, delete: true },
      composedbundles: { get: true, list: true, create: true, update: true, delete: true },
    },
    scopes: scopes ?? [],
  };
}

export function createMeResponse(
  overrides?: Partial<MeResponse>,
): MeResponse {
  return {
    subject: "user:test@example.com",
    preferredUsername: "testuser",
    email: "test@example.com",
    name: "Test User",
    groups: ["developers"],
    displayName: "Test User",
    authMode: "local",
    ...overrides,
  };
}

export function createAuthConfig(overrides?: Partial<AuthConfig>): AuthConfig {
  return {
    mode: "local",
    localAccounts: false,
    ...overrides,
  };
}

// ---- ActivityEvent ----

export function createActivityEvent(
  overrides?: Partial<ActivityEvent>,
): ActivityEvent {
  return {
    timestamp: new Date().toISOString(),
    type: "ControllerCreated",
    source: "varroa-jenkins",
    controller: "test-ctrl",
    namespace: "default",
    message: "Controller test-ctrl created",
    ...overrides,
  };
}

// ---- Pipelines ----

/** createPipelineSummary sets every required field of PipelineSummary,
 * including kind. */
export function createPipelineSummary(
  overrides?: Partial<PipelineSummary>,
): PipelineSummary {
  return {
    jobPath: "payments/app-a",
    health: "unknown",
    kind: "pipeline",
    pending: false,
    ...overrides,
  };
}

/** createRunningRun sets every required field of RunningRun, which carries
 * no kind: a running build names a job, never an item declaration. */
export function createRunningRun(overrides?: Partial<RunningRun>): RunningRun {
  return {
    runId: nextId("run"),
    jobPath: "payments/app-a",
    status: "RUNNING",
    ...overrides,
  };
}

// ---- Fleet locks ----

/** createFleetLock sets every required field of FleetLock. runLink is
 * absent by default, because the BFF sends it only to a caller whose
 * grant covers the run. A waiting row carries waitingSince, and a held or
 * a releasing row carries acquiredAt, as the BFF answers them. */
export function createFleetLock(overrides?: Partial<FleetLock>): FleetLock {
  const state = overrides?.state ?? "held";
  const since = state === "waiting"
    ? { waitingSince: "2026-01-01T00:00:00Z" }
    : { acquiredAt: "2026-01-01T00:00:00Z" };
  return {
    name: "payments/smoke-lock",
    cluster: "core",
    tenant: "payments",
    jobPath: "payments/app-a",
    state,
    runId: nextId("run"),
    ...since,
    ...overrides,
  };
}
