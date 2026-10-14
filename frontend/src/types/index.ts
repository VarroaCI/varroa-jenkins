import type { TenantScope } from "./auth";

export type ControllerPhase = "Pending" | "Provisioning" | "Running" | "Connected" | "Stopped" | "Failed" | "Hibernated";

export type ControllerAttentionKind =
  | "failed"
  | "reconcileBlocked"
  | "bootFailed"
  | "pluginRollFailed"
  | "applyFailed"
  | "miteUnreachable";

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
  /** Hash of the reviewed content; the approve call sends it as expectedContentHash. */
  contentHash?: string;
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
  /** The scope's pipelines-tenant allowlist. A scope names one dimension:
   * tenants never combines with namespaces or controllerSelector. */
  tenants?: TenantScope[];
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
  status?: VarroaRoleStatus;
}

/**
 * Written by the core BFF federation pass only. A request never sets it. It
 * holds an entry only for a hive where the delete of a stale federated
 * JenkinsRole failed.
 */
export interface VarroaRoleStatus {
  observedGeneration?: number;
  federation?: BindingFederationStatus[];
}

export interface VarroaRoleList {
  items: VarroaRole[];
}

export interface VarroaRoleBindingSpec {
  subjects: SubjectRef[];
  roleRef: string;
  scope?: VarroaRoleBindingScope;
}

export type BindingFederationState = "federated" | "skipped" | "failed";

/** The federation outcome of a binding, or a fault of a role, on one hive cluster. */
export interface BindingFederationStatus {
  cluster: string;
  state: BindingFederationState;
  reason?: string;
  message?: string;
  lastTransitionTime: string;
}

/** Written by the core BFF federation pass only. A request never sets it. */
export interface VarroaRoleBindingStatus {
  observedGeneration?: number;
  federation?: BindingFederationStatus[];
}

export interface VarroaRoleBinding {
  apiVersion: "varroa.dev/v1alpha1";
  kind: "VarroaRoleBinding";
  metadata: ObjectMeta;
  spec: VarroaRoleBindingSpec;
  status?: VarroaRoleBindingStatus;
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

// ---- Team pipelines tenancy (spec.pipelines / status.placement) ----

/**
 * A Team's pipelines tenancy declaration (spec.pipelines). A Team that sets
 * this becomes a pipelines tenant: one top-level folder on exactly one cell.
 */
export interface TeamPipelines {
  /** Named caps on builds of a set of jobs. A tenant pipelines admin may write this field. */
  concurrencyLimits?: TeamConcurrencyLimit[];
  /** JenkinsVersionProfile the placement pass resolves a cell against. Required. */
  profile: string;
  /**
   * The CellPool the placement pass selects a cell from. Empty means the
   * profile's default pool: the pool whose name equals profile. A tenant
   * that needs a cell of its own names a pool whose maxTenantsPerCell is 1.
   */
  cellPool?: string;
  /** Kubernetes namespace this tenant's agents run in. Must be one of the team's own namespaces. Required. */
  agentNamespace: string;
  /**
   * The ComposedBundle this tenant's items come from. A Team is
   * cluster-scoped and has no namespace to default to, so the caller sets
   * both name and namespace explicitly.
   */
  composedBundleRef?: { name: string; namespace?: string };
  /**
   * How many of a job's newest completed builds the operator seeds when
   * this tenant's cell changes. The newest successful build is always
   * seeded in addition. Default 10, range 0 to 100.
   */
  rehydrateBuilds?: number;
  /**
   * How long the operator keeps retrying a deferred rehydration seed
   * before it stops and reports RehydrationFailed. Default "30m0s", range
   * 1m to 24h.
   */
  rehydrateDeadline?: string;
  /**
   * Whether an interrupted run retries on its own. "manual" (default)
   * leaves the retry to a user. "automatic" retries once.
   */
  interruptedRetry?: "manual" | "automatic";
  /**
   * This tenant's job concurrency policy. "allow" (default) runs builds
   * side by side. "cancelInProgress" cancels the running build of the
   * same job path when a new one starts.
   */
  concurrency?: "allow" | "cancelInProgress";
  /** Turns the backstop cron scheduler on for this tenant. Default true. */
  cronBackstop?: boolean;
  /**
   * How many missed cron occurrences the backstop fires on wake. "one"
   * (default) fires the newest missed occurrence. "none" fires nothing.
   */
  cronCatchUp?: "one" | "none";
  /**
   * How long the backstop waits past a missed cron occurrence before it
   * fires. Default "5m0s", range 1m to 60m.
   */
  cronGrace?: string;
  /**
   * Active slot count. Default 1, range 1 to 16. This field is not the team's member list.
   */
  members?: number;
  /** Warm standby members. Default 0, range 0 to 8. */
  standby?: number;
  /**
   * Grants of this tenant's jobs and locks to other tenants. An absent
   * list exports nothing.
   */
  exports?: TeamExport[];
  /** Outward notifications. The completion webhook is set through the API, not the form. */
  notifications?: TeamNotifications;
  /** Record-store windows for this tenant's runs. Only an operator sets it. */
  retention?: TeamPipelinesRetention;
}

/** Outward notifications of a pipelines tenant. */
export interface TeamNotifications {
  /** Completion webhook. The record store posts a signed payload for each run. */
  webhook?: TeamWebhook;
}

/** A completion webhook. The signing key lives in the Secret that secretRef names. */
export interface TeamWebhook {
  /** Receiver endpoint. Starts with https:// or http://. */
  url: string;
  secretRef: { name: string; key?: string };
}

/** A cap on the builds of a named set of jobs that run at once. */
export interface TeamConcurrencyLimit {
  name: string;
  /** Folder-relative job globs (*, **, ?). */
  jobs: string[];
  max: number;
}

/** The concurrency cap that holds a queued run. */
export interface RunWait {
  kind: "tenant" | "cell";
  /** The tenant limit's name. Absent for a cell cap. */
  limit?: string;
  max: number;
  running: number;
  cell?: string;
  since: string;
}

/** A queued run of a job that a concurrency cap holds. */
export interface RunWaitEntry extends RunWait {
  runId: string;
}

/** The tenant window and the per-job rules of spec.pipelines.retention. */
export interface TeamPipelinesRetention {
  /** Tenant window in whole days, such as "30d". Shortens the store window only. */
  runs?: string;
  /** Per-job windows. The first matching glob sets the window of that job. */
  jobs?: TeamJobRetention[];
}

/** One per-job window rule of spec.pipelines.retention. */
export interface TeamJobRetention {
  /** Folder-relative job glob. It must not start with /. */
  job: string;
  /** Job window in whole days, such as "7d". */
  runs: string;
}

/**
 * One grant a pipelines tenant makes to other tenants: a job pattern or a
 * list of lock patterns, plus the callers allowed to use it. Exactly one
 * of job or locks is set.
 */
export interface TeamExport {
  /** Folder-relative job glob this tenant exports. Exactly one of job or locks is set. */
  job?: string;
  /** Prefix-relative lock-name globs this tenant exports. Exactly one of job or locks is set. */
  locks?: string[];
  /** Team names allowed to use this export. */
  callers: string[];
}

/**
 * The cell a pipelines tenant is bound to (status.placement). Response-only:
 * the placement pass is its only writer.
 */
export interface TeamPlacement {
  /** Cluster the bound cell runs in. */
  cluster?: string;
  /** Name of the bound cell Controller. */
  cell?: string;
  /** Namespace of the bound cell Controller. */
  cellNamespace?: string;
  /** Increases only when the bound cell changes. */
  generation?: number;
  /**
   * Every member of this tenant. The scalar fields above keep describing
   * slot 0, so a page that renders the member list shows every cell and
   * not slot 0's alone.
   */
  members?: TeamMember[];
}

/** One member of a pipelines tenant (status.placement.members). */
export interface TeamMember {
  /** Name of the cell Controller that holds this member. */
  cell?: string;
  /** Namespace of that cell Controller. */
  cellNamespace?: string;
  /** "active" or "standby". A standby holds no slot and runs no build. */
  role?: string;
  /** The slot index. Set only on an active member. */
  slot?: number;
  /** True when the member's cell reports this tenant applied. */
  applied?: boolean;
}

/**
 * teamMemberLabel renders one member for a listing: the cell name, and the
 * slot or the standby role. A tenant with no member list falls back to the
 * scalar placement, so a single-member tenant reads as it always has.
 */
export function teamMemberLabel(member: TeamMember): string {
  const role = member.role === "standby" ? "standby" : `slot ${member.slot ?? 0}`;
  return `${member.cell ?? "?"} (${role})`;
}

/**
 * A pipelines tenant's build-record rehydration state (status.rehydration).
 * Response-only: TeamReconciler is its only writer.
 */
export interface TeamRehydration {
  /** Placement generation this state was built for. */
  generation?: number;
  /** Cell pod UID this state was built for. */
  cellInstance?: string;
  /** Number of thin build records the last bundle carried. */
  records?: number;
  /** True when the last bundle dropped changelogs or older records to fit the byte budget. */
  truncated?: boolean;
  /** Number of jobs no bundle has covered yet. */
  unseededJobs?: number;
  /** Job paths the mite could not seed on the last apply, because the job did not exist yet. */
  deferred?: string[];
  /** Rehydration retries since the last applied bundle. */
  attempts?: number;
  /** When the operator last sent a rehydration bundle for this tenant. */
  lastAttempt?: string;
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
  // Exactly one of repoURL or ociRef; the reserved varroa-update-center source carries neither.
  repoURL?: string;
  ociRef?: string;
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
  operatorNamespace?: string;
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
  retentionDays?: number;
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
  clusters?: BroodRunClusterStatus[];
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
  /** Lowest Jenkins core that requests a latest entry. Empty for a pinned version. */
  core?: string;
  /** Set only on unservable entries: NotListed, ArchiveDisabled, or PullThroughDisabled. */
  reason?: string;
}

export interface UpdateCenterPluginRef {
  plugin: string;
  version: string;
}

export interface UpdateCenterStatus {
  enabled: boolean;
  phase: string;
  conditions: UpdateCenterCondition[];
  pluginCount: number;
  storeBytes: number;
  gapCount: number;
  gaps: UpdateCenterGap[];
  unservable: UpdateCenterGap[];
  unservableCount: number;
  /** The first 50 stored plugin versions whose dependency list is unknown. */
  unknownDependencies: UpdateCenterPluginRef[];
  /** Every stored plugin version whose dependency list is unknown. Not truncated. */
  unknownDependencyCount: number;
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

// ---- Pipelines (add-pipelines-ui) ----
// Mirrors api/openapi/components/schemas.yaml exactly; see AGENTS.md's rule
// that this file is hand-maintained, not generated.

export type PipelineHealth = "clear" | "cloudy" | "rain" | "unknown";

/** TriggerSource names the kind of cause that started a run. */
export type TriggerSource = "user" | "webhook" | "cron" | "upstream" | "replay" | "restart" | "scm" | "other";

/** ChangeRequest names the pull request a multibranch run builds. */
export interface ChangeRequest {
  id: string;
  title?: string;
  sourceBranch?: string;
  targetBranch?: string;
  url?: string;
}

/** One branch, change request, or tag of a multibranch job. */
export interface JobBranchHead {
  name: string;
  jobPath: string;
  orphaned: boolean;
  revision?: string;
  changeRequest?: ChangeRequest;
  lastRun?: RunSummary;
}

/** The 202 answer of the scan route. */
export interface ScanRequestedResponse {
  scanId: string;
  state: "requested";
}

/** The last requested scan of a multibranch job. */
export interface BranchScan {
  scanId: string;
  state: "running" | "completed" | "failed" | "timeout" | "refused";
  actor?: string;
  requestedAt?: string;
  endedAt?: string;
  result?: string;
  newHeads: string[];
  changedHeads: string[];
  orphanedHeads: string[];
  message?: string;
}

/** The branch and change-request inventory of a multibranch job. The
 * inventory can be up to ten minutes stale. */
export interface JobBranchList {
  jobPath: string;
  observedAt?: string;
  indexedAt?: string;
  indexResult?: string;
  truncated: boolean;
  branches: JobBranchHead[];
  changeRequests: JobBranchHead[];
  tags: JobBranchHead[];
  lastScan?: BranchScan;
}

/** One run, condensed for a list or a card. */
export interface RunSummary {
  runId: string;
  status: string;
  buildNumber?: number;
  jobPath?: string;
  result?: string;
  branch?: string;
  scmRevision?: string;
  actor?: string;
  cause?: string;
  scmUrl?: string;
  /** The forge that hosts scmUrl. Absent when the forge is unknown. */
  scmForge?: "github" | "gitlab" | "gitea";
  triggerSource?: TriggerSource;
  changeRequest?: ChangeRequest;
  queuedAt?: string;
  startedAt?: string;
  endedAt?: string;
  durationMs?: number;
}

/**
 * One unfinished run for the team home's Running now card. currentStage
 * names the top-level stage in progress. It is absent when no top-level
 * stage is in progress: before the first stage, between two stages, and
 * for a queued run.
 */
export interface RunningRun {
  runId: string;
  jobPath: string;
  status: string;
  buildNumber?: number;
  currentStage?: string;
  branch?: string;
  scmRevision?: string;
  changeRequest?: ChangeRequest;
  changeSummary?: string;
  queuedAt?: string;
  startedAt?: string;
  typicalDurationMs?: number;
}

/**
 * The item kind the tenant's composed bundle declares for a job. A job the
 * record store alone holds reports "unknown", and so does a declared item
 * of any other kind.
 */
export type PipelineItemKind = "pipeline" | "multibranch" | "unknown";

/**
 * One job's run-history summary for the Pipelines table. A job that has
 * never run carries no lastBuild. medianQueueWaitMs is absent when no run
 * in the window started.
 */
export interface PipelineSummary {
  jobPath: string;
  health: PipelineHealth;
  kind: PipelineItemKind;
  /** True when every tenant that declares this job reports a complete job list that lacks it. */
  pending: boolean;
  lastBuild?: RunSummary;
  durationTrendMs?: number[];
  medianQueueWaitMs?: number;
}

/** GET /pipelines/{team}: the job list plus the running-now strip. */
export interface PipelineListResponse {
  items: PipelineSummary[];
  runningNow: RunningRun[];
  /** The Jenkins folder that prefixes every job path in the response. It is
   * the Team name for a placed pipelines tenant, and the empty string for a
   * controller tenant. The field is always present. */
  tenantFolder: string;
  truncated: boolean;
  /** The record-store windows that apply to this team. */
  retention: PipelineRetention;
}

/** The record-store run windows of one pipelines team, in whole days. */
export interface PipelineRetention {
  /** The store-wide window. It caps every other window. */
  storeDays: number;
  /** The tenant window. Absent when the tenant sets none. */
  tenantDays?: number;
  /** The window a job without a matching rule gets. */
  effectiveDays: number;
  /** The per-job rules, in match order. */
  jobs: PipelineJobRetention[];
}

/** One per-job window rule, with the days it names and the days it gives. */
export interface PipelineJobRetention {
  job: string;
  /** The window the rule names, before the store cap. */
  days: number;
  /** The window the rule gives, after the store cap. */
  effectiveDays: number;
}

/**
 * Where a job's Jenkinsfile comes from. The catalog template, when one
 * exists, is reported separately on PipelineJobDetail.template — see
 * PipelineTemplateInfo.
 */
/** Where a job's pipeline script comes from. */
export type PipelineDefinitionSource = "jenkinsfile" | "inline" | "template";

export interface PipelineDefinition {
  source: PipelineDefinitionSource;
  jenkinsfileRepo?: string;
  jenkinsfileCommit?: string;
  profile?: string;
  /** Which input of the job's composed bundle declared the job. */
  declaredBy?: JobDeclaredBy;
  /** The Jenkinsfile's script path inside its own repository. The server
   * sets it only for a `jenkinsfile` source whose last run recorded it. */
  definitionPath?: string;
}

/** The composed bundle that owns a declaring input. */
export interface BundleRef {
  namespace: string;
  name: string;
}

/** The catalog item an `itemRef` input references, at the namespace it
 * resolved in. */
export interface CatalogItemRef {
  name: string;
  namespace: string;
}

/**
 * Which input of the job's composed bundle declared the job. `kind` names
 * the input's union branch. `bundle` names the bundle that owns the input.
 */
export interface JobDeclaredBy {
  kind: "gitSource" | "ociSource" | "itemRef";
  name: string;
  revision?: string;
  bundle: BundleRef;
  repoURL?: string;
  path?: string;
  ociRef?: string;
  catalogItem?: CatalogItemRef;
}

/** Where a job's resolved template came from. */
export type PipelineTemplateSource = "marker" | "items";

/**
 * The resolved catalog template for a job, read from the record store
 * (marker-defined) or the items engine and catalog (items-declared).
 * Absent from PipelineJobDetail for a job with no catalog template.
 *
 * For source "marker": name, version, and asOfBuild are absent before the
 * job's first run — markerPath and source are always present. markerRevision
 * is the trusted revision the cell read the marker from, not the branch tip.
 * For source "items": only name, version, and source are set.
 */
export interface PipelineTemplateInfo {
  name?: string;
  version?: string;
  markerPath?: string;
  markerRevision?: string;
  source: PipelineTemplateSource;
  asOfBuild?: number;
}

export interface PipelineTriggers {
  push?: boolean;
  pullRequest?: boolean;
  webhook?: boolean;
  schedule?: string;
  concurrency?: number;
}

export type PipelineParameterKind = "string" | "boolean" | "choice" | "text" | "password";

/**
 * One declared build parameter, converted from the items engine's parameter
 * kind into a JSON Schema fragment the frontend's form renders directly.
 */
export interface PipelineParameter {
  name: string;
  kind: PipelineParameterKind;
  schema: Record<string, unknown>;
  description?: string;
  uiWidget?: "textarea" | "password";
  defaultValue?: string;
}

/** The stage with the greatest median duration across the window's runs. */
export interface PipelineSlowestStage {
  name: string;
  durationMs: number;
}

/**
 * The job's "Last 30 days" card. passRate, medianDurationMs, and
 * slowestStage are absent when the window holds no finished run or no
 * finished stage. medianQueueWaitMs is absent when no run in the window
 * started.
 */
export interface PipelineLast30Days {
  builds: number;
  passRate?: number;
  medianDurationMs?: number;
  medianQueueWaitMs?: number;
  slowestStage?: PipelineSlowestStage;
}

/**
 * One cross-tenant dependency edge (design D13): a producer tenant and
 * job, the result threshold the edge fires on, and whether the
 * producer's exports currently grant it. A declared edge whose grant is
 * missing carries granted false; it is never dropped.
 */
export interface PipelineDependencyEdge {
  tenant: string;
  /** The job's name, relative to the other tenant's folder. A display name. */
  job: string;
  /** The job's path with its tenant folder. The job page link takes this path. */
  jobPath: string;
  result: string;
  granted: boolean;
  /** The edge's most recent run within 30 days. Absent when none exists. */
  lastRun?: RunLink;
}

/**
 * The job's cross-tenant dependency edges. Both arrays are empty, never
 * omitted, for a job that declares no cross-tenant trigger and has no
 * subscriber.
 */
export interface PipelineDependencies {
  upstream: PipelineDependencyEdge[];
  downstream: PipelineDependencyEdge[];
}

/** GET /pipelines/{team}/jobs/{job}. */
export interface PipelineJobDetail {
  /** Queued runs of this job that a concurrency cap holds. */
  waiting?: RunWaitEntry[];
  jobPath: string;
  /** pipeline, multibranch, or unknown. The job page picks its card from it. */
  kind: "pipeline" | "multibranch" | "unknown";
  /** True when every tenant that declares this job reports a complete job list that lacks it. */
  pending: boolean;
  /** The tenant's Jenkins folder prefix (for example "payments"). Empty for
   * a root-hosted tenant. jobPath carries this prefix; strip it before
   * matching a grant's folder-relative job glob. */
  tenantFolder: string;
  /** The tenant noun the server authorizes against: a Team name for a
   * Team tenant, or "<namespace>/<controller>" for a synthetic tenant.
   * Pass this, not the URL team segment, to canAccessTenantPipeline. */
  tenant: string;
  health: PipelineHealth;
  lastBuild?: RunSummary;
  durationTrendMs?: number[];
  definition?: PipelineDefinition;
  triggers?: PipelineTriggers;
  parameters?: PipelineParameter[];
  /** Absent for a job with no catalog template (a Jenkinsfile job). */
  template?: PipelineTemplateInfo;
  /** Null when the record store could not answer the window query. */
  last30Days: PipelineLast30Days | null;
  dependencies: PipelineDependencies;
}

/** GET /pipelines/{team}/jobs/{job}/runs, cross-tenant cursor-merged. */
export interface RunListResponse {
  items: RunSummary[];
  nextCursor?: string;
}

/**
 * POST /pipelines/{team}/jobs/{job}/runs. clientToken, a UUIDv7, makes the
 * request idempotent: the BFF uses it as the run identifier. A request with
 * no clientToken is not idempotent.
 */
export interface CreateRunRequest {
  clientToken?: string;
  parameters?: Record<string, string>;
  /** Breaks a tie when the job path lies in more than one surviving tenant. */
  cluster?: string;
}

/**
 * 202 response for a run request or a rerun. The BFF does not wait for the
 * cell's admission decision; the caller polls the run.
 */
export interface RunActionResponse {
  runId: string;
  intentKey: string;
  state: "requested";
}

/** 202 response for a cancel request. Cancel is fire and forget. */
export interface CancelRunResponse {
  state: "requested";
}

export interface RunStage {
  stageId: string;
  name: string;
  status: string;
  parentId?: string;
  nodeType?: string;
  startedAt?: string;
  endedAt?: string;
  durationMs?: number;
  error?: string;
}

/** GET /runs/{run}. */
export interface RunDetail {
  /** The concurrency cap that holds this queued run. */
  waiting?: RunWait;
  runId: string;
  jobPath: string;
  /** The tenant's Jenkins folder prefix (for example "payments"). Empty for
   * a root-hosted tenant. jobPath carries this prefix; strip it before
   * matching a grant's folder-relative job glob. */
  tenantFolder: string;
  /** The tenant noun the server authorizes against: the run row's
   * stored tenant column. A Team name for a Team tenant, or
   * "<namespace>/<controller>" for a synthetic tenant. Pass this, not
   * the URL team segment, to canAccessTenantPipeline. */
  tenant: string;
  status: string;
  buildNumber?: number;
  branch?: string;
  scmRevision?: string;
  result?: string;
  queuedAt?: string;
  startedAt?: string;
  endedAt?: string;
  durationMs?: number;
  actor?: string;
  cause?: string;
  scmUrl?: string;
  /** The forge that hosts scmUrl. Absent when the forge is unknown. */
  scmForge?: "github" | "gitlab" | "gitea";
  triggerSource?: TriggerSource;
  changeRequest?: ChangeRequest;
  params?: Record<string, string>;
  testsTotal?: number;
  testsFailed?: number;
  testsSkipped?: number;
  /** LogSummary.finalized, read from the run's payload column. */
  logFinalized?: boolean;
  /** The negation of LogSummary.incomplete. */
  logComplete?: boolean;
  /** Set only when result is INTERRUPTED. */
  interruptedReason?: string;
  /** The error text a refused build trigger reported. Set only when result is TRIGGER_FAILED. */
  triggerError?: string;
  /** Why no build exists for the trigger: a controller refusal, CLAIM_EXPIRED, CLAIM_ABANDONED, or a refused build claim (FENCED, CROSS_TENANT, INVALID_KEY, THROTTLED, NOT_HOME). Set only when result is TRIGGER_FAILED. */
  triggerReasonCode?: string;
  /** An existing retry's run id, when one already exists. */
  retryRunId?: string;
  stages?: RunStage[];
  /** How the run was defined, from the record's source column. Absent until
   * a run.source event arrives. */
  source?: RunSource;
  /** The source run this run replays. Absent for a run that is not a replay. */
  replayOf?: string;
  /** The run this run restarted from. Absent for a run that is not a restart. */
  restartOf?: string;
  /** The stage this restarted run began at. Absent for a run that is not a restart. */
  restartStage?: string;
  /** Whether a finished run restarts from a stage, and the stages it can restart from. */
  restart?: RunRestart;
  /** Set only when result is FAILURE and the script sandbox refused a
   * signature. An operator approves the signature on the cell pool. */
  rejectedSignature?: string;
  /** The newest cancel or input answer Jenkins refused for this run.
   * Absent when no refusal exists. A later successful command does not
   * remove it. */
  commandRefusal?: RunCommandRefusal;
  /** The one-line run summary. Absent until the run sets one. */
  summary?: string;
  /** The run's failure digest. Absent when no part has a value. */
  digest?: RunDigest;
  /** The run's pending input steps. Absent when the run holds none. */
  inputs?: RunInput[];
  /** The stored test report. Absent when the run holds no report. */
  testReport?: RunDetailTestReport;
  /** The run's failed and skipped test cases, in the reader's order.
   * Absent when the run holds no case row. */
  cases?: RunTestCase[];
  /** The run that started this run. Absent when no run started it. */
  upstream?: RunLink;
  /** The first page of the runs this run started, newest first. Absent
   * when it started none. Page on with listRunDownstream. */
  downstream?: RunLinkPage;
}

/** One run on the far side of a run link. A run the reader may not read
 * carries its tenant alone. */
export interface RunLink {
  tenant: string;
  jobPath?: string;
  runId?: string;
  buildNumber?: number;
  status?: string;
  result?: string;
}

/** One page of the runs a run started. */
export interface RunLinkPage {
  items: RunLink[];
  nextCursor?: string;
}

/** A refused cancel or input answer, as GET /runs/{run} reports it. */
export interface RunCommandRefusal {
  command: 'cancel' | 'proceed_input' | 'abort_input';
  actor?: string;
  httpStatus?: number;
  /** The refusal text, at most 512 characters. */
  error?: string;
  refusedAt: string;
}

/** The test report as GET /runs/{run} reports it. */
export interface RunDetailTestReport {
  blobKey?: string;
  sizeBytes: number;
  caseCount: number;
}

/** GET /runs/{run}/script. Every field is always present. A run that does
 * not replay answers an empty script and an empty loadedScripts map. */
export interface RunScript {
  /** The main Groovy script the run ran. */
  script: string;
  /** The library scripts the run loaded, by name. Only the main script is editable. */
  loadedScripts: Record<string, string>;
  /** "script" or "template". */
  definitionKind: string;
  replayable: boolean;
  /** Why the run does not replay. Empty for a run that replays. */
  reason: string;
}

/** GET /runs/{run}/restart. Whether a declarative run restarts from a stage. */
export interface RunRestart {
  restartable: boolean;
  /** Why the run does not restart. Absent when restartable is true. */
  reason?: string;
  /** The stages the restart can begin at, in run order. */
  stages: string[];
}

/** One entry of a digest's failed case list. */
export interface RunDigestFailedCase {
  suite: string;
  className: string;
  name: string;
  /** An opaque key. A client passes it back unchanged and never parses it. */
  caseKey: string;
}

/** The failed-test counts of a run digest. */
export interface RunDigestFailedTests {
  failed: number;
  skipped?: number;
  total?: number;
  /** At most ten failed cases, in the reader's order. Absent when the run
   * holds no case row. */
  cases?: RunDigestFailedCase[];
}

/** The failure digest of a run. This is not the content digest of an
 * artifact: RunArtifact.digest is a hash, and this is the run's own failure
 * report. */
export interface RunDigest {
  stage?: string;
  error?: string;
  logTail?: string;
  failedTests?: RunDigestFailedTests;
}

/** One shared library a run was defined with. */
export interface RunSourceLibrary {
  name: string;
  version?: string;
  revision?: string;
  /** The library repository URL. It carries no credentials. */
  repositoryUrl?: string;
  /** The forge that hosts repositoryUrl. Absent when unknown. */
  repositoryForge?: "github" | "gitlab" | "gitea";
}

/** One pod template the run allocated. */
export interface RunSourcePodTemplate {
  name: string;
  label?: string;
  image?: string;
}

/** How a run was defined. */
export interface RunSource {
  /** The pipeline script path in the run's own repository. Absent for a run
   * that a template defines. */
  definitionPath?: string;
  libraries?: RunSourceLibrary[];
  podTemplates?: RunSourcePodTemplate[];
}

/** One parameter a pending input step declares. The BFF answers these
 * fields in camel case, whatever the raw run.input envelope carries. */
export interface RunInputParameter {
  name: string;
  /** The Java simple name of the definition class. The card picks its
   * control from this value. */
  type: string;
  description?: string;
  /** The declared default, rendered as text. Each control starts here. */
  defaultValue?: string;
  /** The declared choices. A non-empty list renders a select, whatever
   * the type. */
  choices?: string[];
}

/** One input step a run waits on, as GET /runs/{run} reports it. */
export interface RunInput {
  /** The step execution's identifier. A write route addresses the input
   * by this value. */
  id: string;
  /** The step's message. The card shows a fallback when it is absent. */
  message?: string;
  /** The step's proceed label. The card falls back to "Proceed". */
  ok?: string;
  /** The step's submitter specification. The card names it. */
  submitter?: string;
  /** The step's submitter parameter name. The card reads it and renders
   * nothing for it. */
  submitterParameter?: string;
  /** The step's own parameter definitions. An empty list renders no
   * form. */
  parameters?: RunInputParameter[];
}

export interface RunArtifact {
  path: string;
  sizeBytes: number;
  /** False when the stored blob key is empty; the download path then answers 409. */
  downloadable: boolean;
  digest?: string;
  contentType?: string;
}

export interface RunArtifactListResponse {
  items: RunArtifact[];
}

export interface RunTestSuite {
  suite: string;
  total: number;
  failed: number;
  skipped: number;
  durationMs?: number;
}

/** One failed or skipped test case of a run. */
export interface RunTestCase {
  suite: string;
  className: string;
  name: string;
  status: string;
  durationMs?: number;
  errorDetails?: string;
  /** An opaque key. A client passes it back unchanged and never parses it. */
  caseKey: string;
}

/** The run's stored test report, which holds every case. */
export interface RunTestReport {
  /** The artifact path of the report, for example ".varroa/tests.json". */
  path: string;
  sizeBytes: number;
  caseCount: number;
  /** False when the stored blob key is empty; the download path then answers 409. */
  downloadable: boolean;
}

export interface RunTestListResponse {
  items: RunTestSuite[];
  /** The failed cases first, then the skipped cases. Never null. */
  cases: RunTestCase[];
  /** Absent when the run holds no report. */
  report?: RunTestReport;
}

/** One recent outcome of one test case of one job. */
export interface TestCaseOutcome {
  runId: string;
  buildNumber?: number;
  suite: string;
  status: string;
  durationMs?: number;
  errorDetails?: string;
  createdAt: string;
  result?: string;
  /** The run's own page path. */
  runLink: string;
}

/** GET /pipelines/{team}/jobs/{job}/tests/{case}. Newest first. */
export interface TestCaseHistoryResponse {
  items: TestCaseOutcome[];
}

/** One SCM change entry, passed through from the run's changes column. */
export type RunChange = Record<string, unknown>;

export interface RunChangeListResponse {
  items: RunChange[];
}

// ---- Live run notifications (the team run stream) ----

/**
 * One frame of the team run stream, named "run", "stage", "lock",
 * "input", or "wait". The recorder publishes the frame, so every field keeps its
 * snake_case name. The stream sends no first frame and replays nothing, so
 * a page refetches when its stream opens. A page reads a frame to decide
 * which read to refresh, and it renders no field of the payload.
 */
export interface RunNotification {
  kind: "run" | "stage" | "lock" | "input" | "wait";
  cluster: string;
  tenant: string;
  job_path: string;
  run_id: string;
  /** Present on a started run; absent on every other frame. */
  build_number?: number;
  /** Empty when the write changed no run state. A page reads it only to
   * decide whether an aggregate query refetches. */
  status?: string;
  /** Empty unless the run ended. No page renders it. */
  result?: string;
  /** The qualified lock name, as <owner-tenant>/<bare-name>. The owner can
   * hold a slash, so never split this value. Present on a "lock"
   * frame only. */
  lock_name?: string;
  /** The lock's state after the write. Present on a "lock" frame only.
   * On a "wait" frame it is waiting or cleared: a cap holds the run, or
   * it changed the count that holds it, or it released the run. */
  state?: string;
  seq: number;
  at: string;
}

// ---- Fleet locks ----

/**
 * One fleet lock row. The BFF answers this shape and nothing wider: the
 * row carries no holder field, because a page groups the list it already
 * holds, and no queue position, because neither read answers one.
 */
export interface FleetLock {
  /** The qualified name, as <tenant>/<bare-name>. */
  name: string;
  /** The cluster of the run that holds or waits on the lock. */
  cluster: string;
  /** The tenant of the run that holds or waits on the lock. */
  tenant: string;
  jobPath: string;
  /** "waiting", "held", or "releasing". A released row is deleted, so no read answers it. */
  state: string;
  runId: string;
  /** The holder run's page. Absent when the caller cannot read that run. */
  runLink?: string;
  /** The time the run began to wait. Present only on a waiting row. */
  waitingSince?: string;
  /** The time the run acquired the lock. Present on a held or a releasing row. */
  acquiredAt?: string;
}

/** The items envelope of GET /pipelines/{team}/locks and GET /locks. */
export interface FleetLockListResponse {
  items: FleetLock[];
}

/** The body of POST /locks/release. reason goes into the audit record. */
export interface FleetLockReleaseRequest {
  name: string;
  runId: string;
  reason: string;
}

/** The released row that POST /locks/release answers. */
export interface FleetLockReleaseResponse {
  name: string;
  runId: string;
  cluster: string;
  tenant: string;
  jobPath: string;
  cellInstance?: string;
  /** Always "released". */
  state: string;
}

/** The cell that still runs a releasing lock's holder. A restart of that
 * cell ends the holder, and the lock then releases. */
export interface FleetLockHolderCell {
  cluster: string;
  namespace: string;
  name: string;
}

/** The 409 body of POST /locks/release. holderCell is present only when a
 * live pod still runs the lock's holder. */
export interface FleetLockReleaseConflict {
  error: string;
  holderCell?: FleetLockHolderCell;
}

// The run log SSE stream (D5) carries six named event kinds. The event
// stream itself has no OpenAPI schema (it is documented in the path's
// description, not in components/schemas.yaml), so these mirror design.md's
// D5 event table for the run-log-viewer page that consumes them.
export type RunLogEventKind = "meta" | "stage" | "line" | "gap" | "live" | "end";

/** Sent once, first: C3's computed manifest plus the first error line. */
export interface RunLogMetaEvent {
  runId: string;
  chunkCount: number;
  byteCount: number;
  droppedBytes: number;
  complete: boolean;
  status: string;
  truncatedAt?: number;
  missingSeq?: number[];
  firstErrorLine?: number;
}

/**
 * A stage boundary. The viewer places a stage header from the stage ids on
 * the lines, not from startLine or frame order. startLine and startOffset
 * have no consumer in the viewer.
 */
export interface RunLogStageEvent {
  stageId: string;
  name: string;
  status: string;
  startOffset: number;
  startLine: number;
}

/** One console line. */
export interface RunLogLineEvent {
  n: number;
  off: number;
  ts: number;
  text: string;
  stageId?: string;
  nodeId?: string;
}

/** A sequence C3's manifest reports as missing. */
export interface RunLogGapEvent {
  seq: number;
  off: number;
}

/** History ended and the live tail began. */
export interface RunLogLiveEvent {
  active: boolean;
}

/** The run stopped and its log is finalized. */
export interface RunLogEndEvent {
  result: string;
  logComplete: boolean;
  incompleteReason?: string;
}

// ---- Runners (external agents; design D1, D12) ----

/**
 * A Runner's mutable configuration (spec.*). team and launcher are
 * immutable after creation.
 */
export interface RunnerSpec {
  team: string;
  launcher: "ssh" | "inbound";
  /** Unprefixed label suffixes; the operator prefixes each one with <team>-. */
  labels?: string[];
  remoteFS: string;
  numExecutors?: number;
  /** The SSH target. Required for launcher ssh; forbidden for inbound. */
  host?: string;
  /** The SSH port. Required for launcher ssh; forbidden for inbound. */
  port?: number;
  /** Name of a Secret in the Team's agent namespace holding the SSH
   * credential. Required for launcher ssh; forbidden for inbound. */
  credentialRef?: string;
  javaPath?: string;
}

/** Status condition for a Runner. Type Ready. */
export interface RunnerCondition {
  type: string;
  status: string;
  lastTransitionTime?: string;
  reason?: string;
  message?: string;
}

/**
 * A Runner's observed state (status.*). status.token is a live door bearer
 * credential; the BFF sends it only to a caller holding runners:create and
 * omits it for every other caller (design D12). The dashboard never reads
 * or renders this field.
 */
export interface RunnerStatus {
  nodeName?: string;
  cell?: string;
  cellNamespace?: string;
  token?: string;
  connected: boolean;
  lastSeen?: string;
  observedGeneration?: number;
  conditions?: RunnerCondition[];
}

/** Full runner object returned from get/list operations. */
export interface RunnerEntry {
  name: string;
  spec: RunnerSpec;
  status: RunnerStatus;
}

/** Request body for POST /runners. */
export interface RunnerCreateRequest {
  name: string;
  spec: RunnerSpec;
}

/**
 * The four load counts of one tenant on a cell, or the same four summed
 * over the cell. runningNow carries no time bound, so a build that runs
 * longer than a day still counts. runsLast24h and failedLast24h count
 * inside the fixed 24-hour window. jobs counts distinct job paths.
 */
export interface CellUsageCounts {
  runningNow: number;
  runsLast24h: number;
  failedLast24h: number;
  jobs: number;
}

/** One tenant on a cell, with that tenant's role and four counts. */
export interface CellTenantUsage extends CellUsageCounts {
  tenant: string;
  /**
   * That tenant's role on this cell: "slot <n>" for an active member and
   * "standby" for a standby. A standby holds the tenant folder and runs no
   * build, so its running count is zero.
   */
  role?: string;
  /**
   * The namespace of the tenant's build agents, as the tenant's own cluster
   * last announced it. Absent until an announcement names one.
   */
  agentNamespace?: string;
  /** The generation of the tenant's placement row in the record store. */
  placementGeneration: number;
}

/**
 * One cell's load, tenant by tenant. An absent field is absent from the
 * object: queueLength reads as zero only for an empty queue, and a cell in
 * a remote cluster carries no queueLength at all.
 */
export interface CellUsage {
  cluster: string;
  namespace: string;
  name: string;
  windowStart: string;
  cellPool?: string;
  profile?: string;
  jenkinsVersion?: string;
  jenkinsHealth?: string;
  queueLength?: number;
  queueLengthAt?: string;
  tenants: CellTenantUsage[];
  totals: CellUsageCounts;
  /**
   * The effective tenant cap this surface resolves: the cell value when it
   * is not zero, else the owning pool's maxTenantsPerCell, else the chart
   * value, else ten. It is never a minimum over the tiers.
   */
  maxTenants: number;
  freeSlots: number;
}

/** The items envelope of GET /cells/usage. */
export interface CellUsageListResponse {
  items: CellUsage[];
}

/**
 * One row of GET /pipelines, the landing-page list. cell and cellNamespace
 * are set only on a placed row.
 */
export interface PipelineTeam {
  name: string;
  cluster: string;
  cell?: string;
  cellNamespace?: string;
  placed: boolean;
}

/**
 * The body of GET /pipelines. recordStore is false when the list carries
 * no Team that another cluster holds, because the record store is off.
 */
export interface PipelineTeamList {
  items: PipelineTeam[];
  recordStore: boolean;
}

/** The placement state of a team's pipelines tenant. */
export type TeamCellState = "placed" | "moving" | "rehydrating" | "unplaced" | "notCellHosted";

/**
 * The tenant's own view of the cell that hosts it. For the states unplaced
 * and notCellHosted every field other than state and reason is absent. The
 * body carries no other tenant's name, job path, run, or count.
 */
export interface TeamCell {
  state: TeamCellState;
  reason?: string;
  cell?: string;
  namespace?: string;
  cluster?: string;
  cellPool?: string;
  profile?: string;
  jenkinsVersion?: string;
  freeSlots?: number;
  queueLength?: number;
  runningNow?: number;
  runsLast24h?: number;
  failedLast24h?: number;
  jobs?: number;
  /** The number of other tenants on the cell. A count, never a list. */
  otherTenantCount?: number;
  /**
   * True when the caller also holds the controller read grant on the cell.
   * The hint gates no other field of this body.
   */
  canOpenCell: boolean;
  /**
   * One entry for each member of the tenant, ordered by role: active
   * members by ascending slot first, then standbys by cell name. Absent
   * for unplaced and notCellHosted. Every other field of this body
   * describes the tenant's slot 0.
   */
  members?: TeamCellMember[];
}

/**
 * One member of the tenant, with that member's own four load counts. The
 * counts apply the same grant narrowing the top-level counts apply, and no
 * entry names another tenant.
 */
export interface TeamCellMember {
  cell: string;
  namespace: string;
  cluster: string;
  /** "slot <n>" for an active member and "standby" for a standby. */
  role: string;
  runningNow: number;
  runsLast24h: number;
  failedLast24h: number;
  jobs: number;
}

/** One status condition of a CellPool. The only type is PoolReady. */
export interface CellPoolCondition {
  type: string;
  status: string;
  lastTransitionTime?: string;
  reason?: string;
  message?: string;
  observedGeneration?: number;
}

/**
 * A CellPool's configuration. profileRef is immutable, because a pool's
 * cells carry the profile label they were created with.
 */
export interface CellPoolSpec {
  /** Cap on the builds one tenant runs at once on one cell. Zero means no cap. */
  maxRunsPerTenant?: number;
  profileRef: string;
  warm?: number;
  /** The cap on the pool's cells, placed cells included. Zero is unlimited. */
  maxCells?: number;
  /**
   * The capacity a pool cell uses when its own spec.cell.maxTenants is
   * zero. A reported maxTenants differs from it only when the cell's own
   * value wins.
   */
  maxTenantsPerCell?: number;
  unreadyGracePeriod?: string;
  cellTemplate?: Record<string, unknown>;
  /** The trusted shared libraries every cell of the pool serves as global libraries. */
  libraries?: CellPoolLibrary[];
  /** The script-security signatures every cell of the pool approves. */
  approvedSignatures?: string[];
  /** The SMTP setting every cell of the pool renders. The read carries the login Secret name only. */
  mail?: CellPoolMail;
  /** The Gitea and Forgejo servers every cell of the pool declares. Read-only through the API. */
  giteaServers?: CellPoolGiteaServer[];
}

/** One Gitea or Forgejo server of a pool. It carries no credential. */
export interface CellPoolGiteaServer {
  serverUrl: string;
  displayName?: string;
  aliasUrl?: string;
}

/** The SMTP setting of a pool. Read-only through the API. */
export interface CellPoolMail {
  host: string;
  /** Zero or unset means 587. */
  port?: number;
  /** Unset means starttls. */
  security?: 'none' | 'starttls' | 'tls';
  from: string;
  replyTo?: string;
  defaultSuffix?: string;
  /** The Secret in the operator namespace that holds the login. The name only. */
  credentialRef?: string;
}

/** A trusted shared library of a pool. Only the operator declares it. */
export interface CellPoolLibrary {
  name: string;
  remote: string;
  defaultVersion: string;
  /** The Secret in the operator namespace that holds the login. Required for an ssh remote. */
  credentialRef?: string;
  implicit?: boolean;
  /** Lets a pipeline load another version. Anyone who can push to the repository then runs trusted code. */
  allowVersionOverride?: boolean;
}

/** A CellPool's reported cell and tenant counts. */
export interface CellPoolStatus {
  observedGeneration?: number;
  /** Owned cells that hold no tenant. */
  warmCells: number;
  /** Owned cells that hold at least one tenant. */
  placedCells: number;
  unreadyCells: number;
  /** The spare capacity of the placed cells only. warmCells counts the empty ones. */
  freeTenantSlots: number;
  tenants?: string[];
  conditions?: CellPoolCondition[];
}

/** One cell pool, as the read and scale surface returns it. */
export interface CellPool {
  /** The cluster that holds the pool. A pool name is unique inside one cluster only. */
  cluster: string;
  name: string;
  spec: CellPoolSpec;
  status: CellPoolStatus;
}

/** The read status of one cluster in GET /cellpools. */
export interface CellPoolClusterStatus {
  name: string;
  ok: boolean;
  error?: string;
}

/**
 * The body of GET /cellpools. clusters holds one entry for each cluster the
 * list read. A cluster with ok false added no item.
 */
export interface CellPoolListResponse {
  items: CellPool[];
  clusters: CellPoolClusterStatus[];
}

/** The body of PATCH /cellpools/{name}. It carries the four scale dials only. */
export interface CellPoolScaleRequest {
  spec: {
    warm?: number;
    maxCells?: number;
    maxTenantsPerCell?: number;
    unreadyGracePeriod?: string;
  };
}

/** Where a grant on a tenant comes from. */
export type CellGrantSource = "teamMembership" | "tenantScopedBinding" | "clusterWideBinding";

/** One subject of a grant. */
export interface CellGrantSubject {
  kind: string;
  name: string;
}

/**
 * One grant that reaches a tenant of a cell. An absent jobs field and an
 * empty jobs field mean different things, so the field is never an empty
 * array: an absent field narrows nothing.
 */
export interface CellGrant {
  source: CellGrantSource;
  /** The VarroaRoleBinding name. Absent for the teamMembership source. */
  binding?: string;
  roleRef: string;
  subjects: CellGrantSubject[];
  verbs: string[];
  jobs?: string[];
}

/** Every grant that reaches one tenant, with its generated Jenkins item role. */
export interface CellTenantGrants {
  tenant: string;
  itemRole: string;
  itemPattern: string;
  /** True only when the 50-grant cap dropped a grant of this tenant. */
  truncated: boolean;
  grants: CellGrant[];
}

/** One entry for each tenant of a cell, naming the subjects that may build on it. */
export interface CellGrants {
  cluster: string;
  namespace: string;
  cell: string;
  tenants: CellTenantGrants[];
}
