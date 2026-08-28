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
    ...overrides,
  };
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
