export type ControllerPhase = "Pending" | "Provisioning" | "Running" | "Connected" | "Stopped" | "Failed" | "Hibernated";

export type ControllerAttentionKind =
  | "failed"
  | "reconcileBlocked"
  | "bootFailed"
  | "pluginRollFailed"
  | "applyFailed";

/** Why a controller needs an operator's attention; absent when healthy. */
export interface ControllerAttention {
  kind: ControllerAttentionKind;
  reason?: string;
  message?: string;
  since?: string;
}

// ---- K8s-style types (matches CRD structure) ----

export interface ObjectMeta {
  name: string;
  namespace?: string;
  creationTimestamp?: string;
  resourceVersion?: string;
  uid?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
}

export interface PluginEntry {
  artifactId: string;
  version: string;
}

export interface PluginSpec {
  policy?: string;
  entries?: PluginEntry[];
}

export interface BackupSpec {
  enabled?: boolean;
  schedule?: string;
  retentionDays?: number;
}

export interface IngressSpec {
  host?: string;
  tlsSecretName?: string;
  mode?: string;
  annotations?: Record<string, string>;
  ingressClassName?: string;
}

export interface ResourceRequirements {
  requests?: { cpu?: string; memory?: string };
  limits?: { cpu?: string; memory?: string };
}

export interface PersistenceSpec {
  size?: string;
  storageClass?: string;
}

export interface MiteSpec {
  resources?: ResourceRequirements;
  image?: string;
  imagePullPolicy?: "Always" | "Never" | "IfNotPresent";
}

export interface MiteStatus {
  connected: boolean;
  version?: string;
  jenkinsVersion?: string;
  jenkinsHealth?: string;
  lastSeen?: string;
  certExpiry?: string;
  lastHealthCheck?: string;
}

export interface ControllerCondition {
  type: string;
  status: string;
  lastTransitionTime?: string;
  reason?: string;
  message?: string;
}

export type ReconciliationMode = "automatic" | "manual" | "idle";

export interface ReconciliationPolicy {
  mode?: ReconciliationMode;
  interval?: string; // Go duration string, e.g. "30s", "5m"
  maxDeferSeconds?: number;
  drainTimeoutSeconds?: number;
  rolloutWave?: number;
}

export interface PendingRestart {
  detectedAt: string;
  desiredStateHash: string;
  changes?: string[];
}

export interface PendingDeletion {
  path: string;
  reason: string;
  detectedAt: string;
}

export interface LiveDrift {
  detected: boolean;
  liveConfigHash?: string;
  detectedAt?: string;
}

export interface ReconcileBlocked {
  blocked: boolean;
  reason?: string;
  message?: string;
  since?: string;
}

export interface RolloutStatus {
  targetBundleHash?: string;
  blocked: boolean;
  paused?: boolean;
  waitingOn?: string[];
  blockedSince?: string;
}

export interface ApplySectionResult {
  name: string;
  ok: boolean;
  error?: string;
}

export interface ApplyResult {
  hash: string;
  timestamp: string;
  succeeded: boolean;
  sections: ApplySectionResult[];
  trigger?: string;
}

export interface ControllerDiff {
  incoming: {
    jcasc: string;
    items: string;
    plugins: string;
  };
  applied?: {
    jcasc: string;
    items: string;
    plugins: string;
  } | null;
  appliedUnavailable?: boolean;
}

export interface RBACGroupBinding {
  name: string;
  role: string;
}

export interface ControllerSpec {
  version?: string;
  className?: string;
  composedBundleRef?: { name: string; namespace?: string };
  pluginSpec?: PluginSpec;
  backupSpec?: BackupSpec;
  ingressSpec?: IngressSpec;
  resources?: ResourceRequirements;
  persistence?: PersistenceSpec;
  miteSpec?: MiteSpec;
  powerState?: string;
  reconciliationPolicy?: ReconciliationPolicy;
  rbacSpec?: { groups: RBACGroupBinding[] };
  podOverrides?: PodOverrides;
  probes?: ProbesSpec;
  resourceOverlay?: ResourceOverlay;
  hibernation?: HibernationSpec;
}

export interface HibernationSpec {
  enabled?: boolean;
  gracePeriodMinutes?: number;
  activityIgnoreRegex?: string;
}

export interface ProbeSpec {
  disabled?: boolean;
  initialDelaySeconds?: number;
  periodSeconds?: number;
  timeoutSeconds?: number;
  failureThreshold?: number;
  successThreshold?: number;
}

export interface ProbesSpec {
  startup?: ProbeSpec;
  readiness?: ProbeSpec;
  liveness?: ProbeSpec;
}

export const PROBE_DEFAULTS = {
  startup: { initialDelaySeconds: 10, periodSeconds: 10, timeoutSeconds: 5, failureThreshold: 30, successThreshold: 1 },
  readiness: { initialDelaySeconds: 0, periodSeconds: 10, timeoutSeconds: 5, failureThreshold: 3, successThreshold: 1 },
  liveness: { initialDelaySeconds: 0, periodSeconds: 10, timeoutSeconds: 5, failureThreshold: 6, successThreshold: 1 },
} as const;

// ---- Resource override / overlay preview types ----

// PodOverrides mirrors the operator's typed pod-scoped customizations. The UI
// edits this as a free-form JSON block, so the shape is intentionally loose;
// the backend re-validates the concrete fields.
export type PodOverrides = Record<string, unknown>;

// ResourceOverlay holds raw strategic-merge-patch YAML strings per resource.
export interface ResourceOverlay {
  statefulSet?: string;
  service?: string;
  ingress?: string;
}

export type OverlayResourceKind = "statefulSet" | "service" | "ingress";

export type OverlayBaseline = "live" | "base";

// OverlayWarning is a warn-but-allow guardrail hit returned by the preview.
export interface OverlayWarning {
  resource: string;
  path: string;
  message: string;
}

export interface PreviewRequest {
  podOverrides?: PodOverrides;
  resourceOverlay: ResourceOverlay;
  baseline?: OverlayBaseline;
}

export interface PreviewResponse {
  merged: Partial<Record<OverlayResourceKind, string>>;
  diff: Partial<Record<OverlayResourceKind, string>>;
  warnings: OverlayWarning[];
  baselineUsed: OverlayBaseline;
}

export interface ControllerStatus {
  phase?: ControllerPhase;
  endpoint?: string;
  // Hibernation is reported in status, not requested in spec: spec.powerState
  // only ever carries Running or Stopped.
  hibernated?: boolean;
  hibernatedAt?: string;
  conditions?: ControllerCondition[];
  miteStatus?: MiteStatus;
  desiredStateHash?: string;
  configHash?: string;
  rbacHash?: string;
  provisioningStartedAt?: string;
  firstConnectedAt?: string;
  lastReconciledAt?: string;
  pendingRestart?: PendingRestart;
  pendingItemDeletions?: PendingDeletion[];
  liveDrift?: LiveDrift;
  rollout?: RolloutStatus;
  appliedBundleHash?: string;
  lastApplyResult?: ApplyResult;
  applyHistory?: ApplyResult[];
}

export interface Controller {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "Controller";
  metadata: ObjectMeta;
  spec: ControllerSpec;
  status: ControllerStatus;
  cluster?: string;
}

export interface ControllerList {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "ControllerList";
  items: Controller[];
  metadata?: { resourceVersion?: string; continue?: string };
  clusters?: Array<{name: string; ok: boolean; error?: string}>;
}

// ---- RBAC types (VarroaRole / VarroaRoleBinding) ----

export type SubjectKind = "Group" | "User";

export interface SubjectRef {
  kind: SubjectKind;
  name: string;
}

export interface VarroaRoleBindingScope {
  namespaces?: string[];
  controllerSelector?: {
    matchLabels?: Record<string, string>;
    matchExpressions?: Array<{
      key: string;
      operator: "In" | "NotIn" | "Exists" | "DoesNotExist";
      values?: string[];
    }>;
  };
}

export interface APIRule {
  resources: string[];
  verbs: string[];
}

export interface VarroaRoleSpec {
  apiRules?: APIRule[];
  jenkinsPermissions?: string[];
}

export interface VarroaRole {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "VarroaRole";
  metadata: ObjectMeta;
  spec: VarroaRoleSpec;
}

export interface VarroaRoleList {
  items: VarroaRole[];
}

export interface VarroaRoleBindingSpec {
  subjects: SubjectRef[];
  roleRef: string;
  scope?: VarroaRoleBindingScope;
}

export interface VarroaRoleBinding {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "VarroaRoleBinding";
  metadata: ObjectMeta;
  spec: VarroaRoleBindingSpec;
}

export interface VarroaRoleBindingList {
  items: VarroaRoleBinding[];
}

// ---- Jenkins RBAC types (JenkinsRole / JenkinsRoleBinding) ----

export type JenkinsRoleType = "Global" | "Item" | "Agent";

export interface JenkinsRoleSpec {
  roleType: JenkinsRoleType;
  permissions: string[];
  description?: string;
}

export interface JenkinsRole {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "JenkinsRole";
  metadata: ObjectMeta;
  spec: JenkinsRoleSpec;
}

export interface JenkinsRoleList {
  items: JenkinsRole[];
}

export interface JenkinsScope {
  type?: string; // "Global" | "Folder" | "Pattern"
  folder?: string;
  propagate?: "None" | "Children" | "Subtree";
  pattern?: string;
}

export interface JenkinsRoleBindingSpec {
  subjects: SubjectRef[];
  roleRef: string;
  controllerScope?: VarroaRoleBindingScope;
  jenkinsScope?: JenkinsScope;
}

export interface JenkinsRoleBinding {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "JenkinsRoleBinding";
  metadata: ObjectMeta;
  spec: JenkinsRoleBindingSpec;
}

export interface JenkinsRoleBindingList {
  items: JenkinsRoleBinding[];
}

// ---- CatalogSource ----

export interface TemplateCatalogCondition {
  type: string;
  status: string;
  lastTransitionTime?: string;
  reason?: string;
  message?: string;
}

export interface CatalogSourceSpec {
  repoURL: string;
  revision?: string;
  path?: string;
  syncIntervalSeconds?: number;
  secretRef?: string;
  trusted?: boolean;
}

export type CatalogSyncPhase = "Pending" | "Syncing" | "Ready" | "Error";

export interface CatalogSourceStatus {
  phase?: CatalogSyncPhase;
  observedRevision?: string;
  lastSyncTime?: string;
  itemCount?: number;
  message?: string;
  conditions?: TemplateCatalogCondition[];
}

export interface CatalogSource {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "CatalogSource";
  metadata: ObjectMeta;
  spec: CatalogSourceSpec;
  status?: CatalogSourceStatus;
}

export interface CatalogSourceList {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "CatalogSourceList";
  items: CatalogSource[];
}

// ---- CatalogItem ----
export type CatalogItemType = "podtemplate" | "plugin" | "item" | "jcasc" | "rbac" | "pipeline-template" | "groovy";

export interface CatalogVariable {
  name: string;
  default?: string;
  description?: string;
  required?: boolean;
  type?: "string" | "number" | "boolean" | "credentials";
  allowedValues?: string[];
}

export interface CatalogItemSpec {
  sourceRef: string;
  type: CatalogItemType;
  displayName?: string;
  description?: string;
  path: string;
  version?: string;
  tags?: string[];
  variables?: CatalogVariable[];
  requires?: string[];
}

/** Where a closure entry's selected version came from. */
export type CatalogItemProvenance = "store" | "lock";

/**
 * One pinned member of a derived item's dependency closure. The operator
 * persists this because `status.content` is a flat plugins.yaml fragment and
 * can express neither direct-vs-transitive nor provenance nor the minimum in
 * force — and the browser cannot re-derive them, having neither the store
 * annotations nor the solver.
 */
export interface CatalogItemClosureEntry {
  artifactId: string;
  version: string;
  direct?: boolean;
  provenance?: CatalogItemProvenance;
  minimum?: string;
}

/** The five advisory verdicts. None of them ever blocks anything. */
export type CatalogItemVerdict =
  | "compatible"
  | "core-too-old"
  | "dep-below-minimum"
  | "lock-too-old"
  | "unknown";

export interface CatalogItemCompat {
  profile: string;
  /** The profile's effective deployed core — resolveVersion when set, else version. */
  jenkinsVersion?: string;
  verdict: CatalogItemVerdict;
  message?: string;
}

export interface CatalogItemStatus {
  content?: string;
  contentHash?: string;
  observedRevision?: string;
  valid: boolean;
  message?: string;
  closure?: CatalogItemClosureEntry[];
  compat?: CatalogItemCompat[];
  conditions?: TemplateCatalogCondition[];
}

/**
 * One profile's lock pins for a derived item's closure entries, joined by the
 * BFF at read time. A closure entry the lock does not mention has NO key in
 * `pins` — distinct from the lock pinning it at the same version.
 */
export interface CatalogItemLockPins {
  profile: string;
  pins: Record<string, string>;
}

/** The catalog-item get route's response: the item plus the lock-pin join. */
export interface CatalogItemDetailResponse {
  item: CatalogItem;
  lockPins?: CatalogItemLockPins[];
}

export interface CatalogItem {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "CatalogItem";
  metadata: ObjectMeta;
  spec: CatalogItemSpec;
  status?: CatalogItemStatus;
}

export interface CatalogItemSummary {
  name: string;
  namespace: string;
  displayName?: string;
  type: CatalogItemType;
  sourceRef: string;
  version?: string;
  description?: string;
  tags?: string[];
  valid: boolean;
  message?: string;
  contentHash?: string;
  variables?: CatalogVariable[];
  /**
   * The update-center plugin this item was derived for. The browser collapses
   * rows by it, so a plugin stored at three versions is one row with a version
   * selector. Absent for items that are not update-center-derived.
   */
  pluginName?: string;
  /** Advisory per-profile verdicts, so a row can be badged without a detail fetch. */
  compat?: CatalogItemCompat[];
}

export interface CatalogItemList {
  apiVersion?: "varroa.dev/v1alpha1";
  kind?: "CatalogItemList";
  items: CatalogItemSummary[];
  operatorNamespace?: string;
}

// ---- ComposedBundle (Phase 2, but add types now) ----
export type ComposedBundlePhase = "Pending" | "Ready" | "Drifted" | "Invalid";

export interface ComposedItemRef {
  name: string;
  namespace?: string;
  pinnedContentHash?: string;
  variables?: Record<string, string>;
}

export interface GitBundleSource {
  repoURL: string;
  path: string;
  revision?: string;
  secretRef?: string;
}

export interface ComposedInput {
  itemRef?: ComposedItemRef;
  gitSource?: GitBundleSource;
}

export interface ComposedBundleSpec {
  displayName?: string;
  description?: string;
  inputs?: ComposedInput[];
  variables?: Record<string, string>;
  jcascMergeStrategy?: string;
}

export interface ComposedBundleStatus {
  phase?: ComposedBundlePhase;
  itemCount?: number;
  resolvedHash?: string;
  missingItems?: string[];
  driftedItems?: string[];
  errors?: string[];
  warnings?: string[];
  message?: string;
  conditions?: TemplateCatalogCondition[];
  inputSummary?: InputSummaryEntry[];
}


/** Preview returned by the compose preview endpoint. */
export interface ComposedBundlePreview {
  bundleYaml: string;
  jenkinsYaml: string;
  pluginsYaml: string;
  itemsYaml: string;
  rbacYaml: string;
  missing: string[];
  drifted: string[];
  warnings: string[];
}

export interface ComposedBundle {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "ComposedBundle";
  metadata: ObjectMeta;
  spec: ComposedBundleSpec;
  status?: ComposedBundleStatus;
  resolvedInputs?: Array<{
    index: number;
    name: string;
    kind: string;
    type?: string;
    namespace?: string;
    revision?: string;
    status: "Resolved" | "Missing" | "Drifted" | "Unknown";
  }>;
}

export interface ComposedBundleList {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "ComposedBundleList";
  items: ComposedBundle[];
}

export interface BroodHealth {
  totalControllers: number;
  readyControllers: number;
  failedControllers: number;
  provisioningControllers: number;
  pendingControllers: number;
  controllers: Controller[];
}

export interface ProvisioningDefaultsSpec {
  rootDomain?: string;
  ingressAnnotations?: Record<string, string>;
  defaultCPU?: string;
  defaultMemory?: string;
  defaultStorage?: string;
  defaultPlugins?: { artifactId: string; version: string }[];
  defaultVersion?: string;
  storageClass?: string;
  storageSizeGB?: number;
  provisioningTimeoutSec?: number;
  defaultReconciliationPolicy?: ReconciliationPolicy;
  upgradePolicy?: "auto" | "manual";
}

export interface ClusterEntry {
  name: string;
  healthy: boolean;
  core: boolean;
  lastHeartbeat: string;
  operatorVersion: string;
  k8sVersion: string;
  controllerCount: number;
  connectedCount: number;
  state: "active" | "draining" | "drained";
  drainStartedAt?: string;
}

export interface ActivityEvent {
  timestamp: string;
  type: string;
  source: string;
  actor?: string;
  controller?: string;
  namespace?: string;
  // cluster identity stamped by the multi-cluster control plane; optional as a defensive wire mirror
  cluster?: string;
  message: string;
  // Jenkins build fields (presentational mirror — added by ingestion sibling)
  itemPath?: string;
  buildNumber?: number;
  result?: string;
  url?: string;
  phase?: string;
  reason?: string;
  severity?: "info" | "warning" | "error";
}

export interface ActivityPage {
  items: ActivityEvent[];
  nextCursor?: string;
  hasMore: boolean;
  retentionMode: "on" | "off";
  retentionDays?: 7 | 30 | 90;
}

export interface ActivityFilters {
  range?: "15m" | "1h" | "6h" | "24h" | "7d" | "custom";
  start?: string;
  end?: string;
  cluster?: string;
  controller?: string;
  namespace?: string;
  source?: string;
  severity?: string;
  actor?: string;
  type?: string;
}

export interface ProvisioningDefaults {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "ProvisioningDefaults";
  metadata: ObjectMeta;
  spec: ProvisioningDefaultsSpec;
}

// ---- API Keys ----

export interface KeyMeta {
  prefix: string;
  created: string;
  lastUsed?: string;
  expires?: string;
  name?: string;
}

// ---- Observability ----

export type ObservabilityProvider = "jenkins-api" | "prometheus" | "opentelemetry";

export type ObservabilitySourceStatus =
  | "not-configured"
  | "intended"
  | "configured"
  | "exposed"
  | "integrated"
  | "degraded"
  | "unavailable"
  | "unknown";

export type ObservabilityLevel = 0 | 1 | 2 | 3;

export interface ObservabilitySource {
  provider: string;
  status: ObservabilitySourceStatus;
  error?: string;
  hints?: Record<string, string>;
}

export interface ObservabilityWarning {
  message: string;
}

export interface ObservabilityFreshness {
  observedAt?: string;
  miteTTL?: number;
  stale: boolean;
}

export interface ObservabilityRecentBuild {
  jobName: string;
  buildNumber: number;
  status: string;
  startedAt?: string;
  durationSeconds?: number;
  url?: string;
}

export interface ObservabilitySummary {
  totalJobs?: number;
  runningBuilds?: number;
  recentBuilds?: ObservabilityRecentBuild[];
}

export interface ControllerObservability {
  sources: ObservabilitySource[];
  capabilities: string[];
  level: ObservabilityLevel;
  levelName: string;
  warnings?: ObservabilityWarning[];
  freshness: ObservabilityFreshness;
  summary?: ObservabilitySummary;
}

export interface VersionCatalogEntry {
  version: string;
  channel: string;
  recommended?: boolean;
  eol?: string;
  name: string;
  pluginSetReady?: boolean;
  pluginCount?: number;
}

export interface VersionProfileCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export interface VersionProfileDetail {
  name: string;
  version: string;
  channel: string;
  recommended?: boolean;
  eol?: string;
  pluginSetRef?: string;
  contentRef?: string;
  pluginCount?: number;
  // Full pinned plugin lines "name@version" (bare name when unversioned),
  // mirrors the BFF `plugins` array; consumed by the version diff (epic change C).
  plugins?: string[];
  hasJcasc: boolean;
  requiredPlugins?: string[];
  conditions: VersionProfileCondition[];
}

// ---- Version candidates (patch-upgrade tracking; ProfileCandidate CRD) ----
// Mirrors the raw ProfileCandidate CRD JSON returned by
// GET /api/v1/version-candidates, not a flattened BFF projection like
// VersionProfileDetail above.

export interface VersionCandidateCondition {
  type: "Resolved" | "ClosureClean" | "CoreCompatOK" | "PluginsServable" | "PreflightChecked" | "Promoted";
  status: "True" | "False" | "Unknown";
  lastTransitionTime?: string;
  reason?: string;
  message?: string;
}

export interface VersionCandidateFailingController {
  namespace: string;
  name: string;
  conflictCount: number;
  message: string;
}

export interface VersionCandidatePreflightSummary {
  controllersChecked: number;
  controllersFailing: number;
  failingControllers?: VersionCandidateFailingController[];
}

export interface VersionCandidateSpec {
  profileRef: string;
  observedVersion: string;
  resolveVersion: string;
  closureContentRef?: string;
}

export interface VersionCandidateStatus {
  phase?: "Pending" | "Ready" | "Promoted" | "Failed" | "Superseded";
  conditions?: VersionCandidateCondition[];
  preflight?: VersionCandidatePreflightSummary;
  promotedAt?: string;
}

export interface VersionCandidate {
  metadata: ObjectMeta;
  spec: VersionCandidateSpec;
  status: VersionCandidateStatus;
}

export interface ClassMiteSpec {
  image?: string;
  imagePullPolicy?: string;
}

export interface ControllerClassSpec {
  nodeSelector?: Record<string, string>;
  tolerations?: Record<string, unknown>[];
  affinity?: Record<string, unknown>;
  securityContext?: Record<string, unknown>;
  podLabels?: Record<string, string>;
  podAnnotations?: Record<string, string>;
  ingressClassName?: string;
  ingressAnnotations?: Record<string, string>;
  resources?: ResourceRequirements;
  persistence?: PersistenceSpec;
  probes?: ProbesSpec;
  mite?: ClassMiteSpec;
  imagePullSecrets?: string[];
  jvmOpts?: string;
}

export interface ControllerClass {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "ControllerClass";
  metadata: ObjectMeta;
  spec: ControllerClassSpec;
}

export interface ControllerClassList {
  items: ControllerClass[];
}

export interface SizePreset {
  name: string;
  cpu: string;
  memory: string;
  storage: string;
}

export interface ProvisioningConfig {
  rootDomain: string;
  /** Dashboard hostname; path-mode ingress requires the controller host to equal it. Empty when the BFF cannot derive it. */
  dashboardHost: string;
  defaultNamespace: string;
  namespaces: string[];
  defaultVersion: string;
  versions: VersionCatalogEntry[];
  sizePresets: SizePreset[];
  injectedVariables: string[];
}

export interface PreflightCheck {
  id: string;
  status: string;
  message: string;
}

export interface InputSummaryEntry {
  kind: string;
  type: string;
  namespace?: string;
}

// ---- Brood operations (cross-cluster logical-run DTOs per design §4) ----

export type BroodVerb = "restart" | "reprovision" | "reconcile" | "stop" | "start" | "executeGroovy" | "upgrade";
export type BroodOrder = "rolloutWave" | "name";
export type BroodFailurePolicy = "FailFast" | "FailTidy" | "FailAtEnd";
export type BroodOperationPhase = "Pending" | "Running" | "Suspended" | "Succeeded" | "Failed" | "Canceled";
export type BroodTargetState = "Pending" | "Dispatched" | "Succeeded" | "Failed" | "Skipped";

export interface BroodTargetStatus {
  namespace: string;
  name: string;
  wave: number;
  state: BroodTargetState;
  reason?: string;
  dispatchedAt?: string;
  finishedAt?: string;
  output?: string;
}

export interface BroodSummary {
  total: number;
  succeeded: number;
  failed: number;
  skipped: number;
}

export interface BroodGroovyItemRef {
  name: string;
  namespace?: string;
  pinnedContentHash?: string;
  variables?: Record<string, string>;
}

export interface BroodGroovyAction {
  script?: string;
  itemRef?: BroodGroovyItemRef;
}

export interface BroodAction {
  verb: BroodVerb;
  groovy?: BroodGroovyAction;
  upgrade?: { targetVersion?: string };
}

export interface BroodTargetFilters {
  phase?: string;
  version?: string;
  bundle?: string;
}

export interface BroodExecution {
  maxParallel?: number;
  order?: BroodOrder;
  failurePolicy?: BroodFailurePolicy;
}

export interface BroodOperationSpec {
  action: BroodAction;
  targets: {
    names?: string[];
    selector?: Record<string, unknown>;
    namespaces?: string[];
    filters?: BroodTargetFilters;
  };
  execution?: BroodExecution;
  suspend?: boolean;
  ttlSecondsAfterFinished?: number;
}

export interface BroodOperationStatus {
  phase?: BroodOperationPhase;
  startedBy?: string;
  startedAt?: string;
  finishedAt?: string;
  reason?: string;
  targets?: BroodTargetStatus[];
  summary?: BroodSummary;
  observedGeneration?: number;
  scriptSnapshotRef?: string;
}

export interface BroodOperation {
  metadata: ObjectMeta;
  spec: BroodOperationSpec;
  status: BroodOperationStatus;
}

// ---- Cross-cluster logical-run DTOs (design §4) ----

export interface BroodRunClusterStatus {
  cluster: string;
  ok: boolean;
  error?: string;
}

export interface BroodRunCluster {
  cluster: string;
  ok: boolean;
  error?: string;
  op?: BroodOperation;
}

export interface BroodRun {
  namespace: string;
  name: string;
  verb: BroodVerb;
  phase: BroodOperationPhase;
  summary: BroodSummary;
  startedBy?: string;
  createdAt?: string;
  clusters: BroodRunCluster[];
}

export interface BroodRunSummaryRow {
  namespace: string;
  name: string;
  verb: BroodVerb;
  phase: BroodOperationPhase;
  summary: BroodSummary;
  startedBy?: string;
  createdAt?: string;
  clusters: string[];
}

export interface BroodListResponse {
  items: BroodRunSummaryRow[];
  clusters: BroodRunClusterStatus[];
}

export interface BroodPreviewTarget {
  namespace: string;
  name: string;
  wave: number;
  applicable: boolean;
  reason?: string;
}

export interface BroodPreviewSection {
  cluster: string;
  ok: boolean;
  error?: string;
  targets?: BroodPreviewTarget[];
}

export interface BroodPreviewResponse {
  clusters: BroodPreviewSection[];
}

export interface CreateBroodRequest {
  namespace?: string;
  spec: BroodOperationSpec;
  clusters?: string[];
}

export interface SuspendBroodRequest {
  suspend: boolean;
}

export interface BroodActionResult {
  cluster: string;
  ok: boolean;
  code?: string;
  error?: string;
}

export interface BroodActionResponse {
  clusters: BroodActionResult[];
}

// ---- Brood schedules (scheduled brood operations) ----

export interface BroodScheduleTemplate {
  targets: {
    names?: string[];
    selector?: Record<string, unknown>;
    namespaces?: string[];
    filters?: BroodTargetFilters;
  };
  action: BroodAction;
  clusters?: string[];
  execution?: BroodExecution;
  ttlSecondsAfterFinished?: number;
}

export interface BroodScheduleSpec {
  schedule: string;
  suspend?: boolean;
  concurrencyPolicy?: string;
  startingDeadlineSeconds?: number;
  successfulJobsHistoryLimit?: number;
  failedJobsHistoryLimit?: number;
  waitForCompletion?: boolean;
  template: BroodScheduleTemplate;
}

export interface BroodScheduleStatus {
  lastScheduleTime?: string;
  lastSuccessfulTime?: string;
  active?: { name?: string; namespace?: string }[];
  reason?: string;
}

export interface BroodSchedule {
  namespace: string;
  name: string;
  cluster?: string;
  spec: BroodScheduleSpec;
  status?: BroodScheduleStatus;
}

export interface CreateBroodScheduleRequest {
  namespace?: string;
  name: string;
  cluster?: string;
  spec: BroodScheduleSpec;
}

export interface BroodScheduleListResponse {
  items?: BroodSchedule[];
}

// ---- Update Center ----

export interface UpdateCenterCondition {
  type: string;
  status: string;
  lastTransitionTime: string | null;
  reason: string;
  message: string;
}

export interface UpdateCenterGap {
  plugin: string;
  version: string;
  requiredBy: string;
}

export interface UpdateCenterStatus {
  enabled: boolean;
  phase: string;
  conditions: UpdateCenterCondition[];
  pluginCount: number;
  storeBytes: number;
  gaps: UpdateCenterGap[];
  lastSyncTime: string | null;
  storageType: string;
  pullThroughEnabled: boolean;
}

export interface UpdateCenterPlugin {
  name: string;
  version: string;
  sha256: string;
  sizeBytes: number;
}

export interface UpdateCenterPlugins {
  enabled: boolean;
  plugins: UpdateCenterPlugin[];
}

export type UpdateCenterClosureStatus =
  | "satisfied-store"
  | "lock-too-old"
  | "declared-not-yet-stored"
  | "planned-fetch"
  | "not-in-store"
  | "unreachable"
  | "metadata-unavailable"
  | "closure-unverifiable";

export interface UpdateCenterClosureEntry {
  name: string;
  /** The highest minimum any path in the closure required. Not a pin. */
  min: string;
  status: UpdateCenterClosureStatus;
  resolvedVersion?: string;
  source?: "store" | "declared" | "upstream";
  /** True when the upload downloaded and stored these bytes. */
  fetched?: boolean;
}

export interface UpdateCenterOptionalDependency {
  name: string;
  min: string;
}

export interface UpdateCenterUploadWarning {
  code: string;
  plugin: string;
  min?: string;
  message: string;
}

export interface UpdateCenterUploadedPlugin {
  name: string;
  version: string;
  sha256: string;
  displayName?: string;
  requiredCore?: string;
}

export interface UpdateCenterUploadResult {
  plugin: UpdateCenterUploadedPlugin;
  dryRun: boolean;
  packRef?: string;
  closure: UpdateCenterClosureEntry[];
  optionalDependencies: UpdateCenterOptionalDependency[];
  warnings: UpdateCenterUploadWarning[];
}

/**
 * One rejecting dependency. The foundInStore/foundDeclared/foundUpstream triple
 * is what makes the diff actionable.
 */
export interface UpdateCenterUnresolvedDependency {
  name: string;
  min: string;
  reason: string;
  foundInStore: string | null;
  foundDeclared: string | null;
  foundUpstream: string | null;
  remediation?: string;
}

export interface UpdateCenterUploadRejection {
  error: string;
  message?: string;
  unresolved?: UpdateCenterUnresolvedDependency[];
}

// ---- Fleet Plugin Inventory (T2.2) ----

export interface FleetPluginVersionCount {
  version: string;
  controllerCount: number;
}

export interface FleetPluginClassCount {
  class: string; // plain string – no union of known labels (R19)
  controllerCount: number;
}

export interface FleetPluginRollupItem {
  name: string;
  controllerCount: number;
  versions: FleetPluginVersionCount[];
  classes: FleetPluginClassCount[];
}

export interface FleetPluginCoverage {
  complete: boolean;
  controllersTotal: number;
  controllersReporting: number;
  controllersStale: number;
  controllersDegraded: number;
  controllersTruncated: number;
  controllersDetailStale: number;
  controllersMissing: FleetPluginMissingController[];
  clustersNotCovered: number;
}

export interface FleetPluginMissingController {
  cluster: string;
  namespace: string;
  name: string;
  reason: string; // "never-reported" | "hibernated" | "stopped" | "not-connected"
}

export interface FleetPluginDrillItem {
  cluster: string;
  namespace: string;
  controller: string;
  version: string;
  class: string; // plain string – no union of known labels (R19)
  source: string;
  collectedAt: string;
  detailPath: string;
  detailStale: boolean;
  stale: boolean;
  degraded: boolean;
  truncated: boolean;
  optionalEdgesDropped: boolean;
  bootstrapApproximate: boolean;
}

export interface FleetPluginsRollup {
  items: FleetPluginRollupItem[];
  coverage: FleetPluginCoverage;
  clusters: Array<{ name: string; ok: boolean; error?: string }>;
}

export interface FleetPluginDrilldown {
  name: string;
  items: FleetPluginDrillItem[];
  versions: FleetPluginVersionCount[];
  coverage: FleetPluginCoverage;
  clusters: Array<{ name: string; ok: boolean; error?: string }>;
}
