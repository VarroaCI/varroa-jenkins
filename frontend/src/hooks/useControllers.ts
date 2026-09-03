import { useQuery } from "@tanstack/react-query";
import { bffFetch } from "./useApi";
import type { PendingRestart, ReconciliationPolicy, ControllerObservability, PendingDeletion, LiveDrift, ReconcileBlocked, RolloutStatus, ApplyResult, PodOverrides, ResourceOverlay, ProbesSpec, ControllerSpec, ControllerAttention } from "../types";

const enc = (s: string) => encodeURIComponent(s);

export interface ControllerListItem {
  cluster: string;
  name: string;
  namespace: string;
  phase: string;
  endpoint: string;
  miteConnected: boolean;
  miteVersion?: string;
  // version is the desired Jenkins version (spec.version); jenkinsVersion is the
  // actual running version. A drift badge is shown when they differ (#242).
  version?: string;
  jenkinsVersion?: string;
  jenkinsHealth?: string;
  composedBundleRef?: { name: string; namespace?: string };
  /**
   * The bundle the controller is actually using, resolved server-side.
   * Read-only, and separate from composedBundleRef because that field is PATCHed
   * back when attaching or detaching — resolving into it would silently pin a
   * zero-config controller to the starter bundle on the next save.
   */
  effectiveBundle?: { name: string; namespace: string; builtIn: boolean };
  routingMode?: "subdomain" | "path";
  appliedBundleHash?: string;
  rolloutWave?: number;
  attention?: ControllerAttention;
  /** Last mite heartbeat, RFC3339 UTC. Absent when no mite has ever reported. */
  lastSeen?: string;
}

export function useControllers() {
  return useQuery({
    queryKey: ["controllers"],
    queryFn: () => bffFetch<{items: ControllerListItem[]}>("/controllers").then(r => r.items),
    refetchInterval: 15_000,
  });
}

export interface ControllerDetail {
  /**
   * The controller's full spec, projected verbatim by the BFF so the spec
   * editor can render and diff it. The flattened fields below remain for
   * older consumers.
   */
  spec: ControllerSpec;
  cluster: string;
  name: string;
  namespace: string;
  phase: string;
  endpoint: string;
  version: string;
  powerState?: string;
  /**
   * Whether the controller is currently hibernated (status.hibernated).
   * Hibernation is a status fact, not a power state: spec.powerState only
   * ever carries Running or Stopped.
   */
  hibernated?: boolean;
  /** When the controller was last hibernated (status.hibernatedAt, RFC 3339). */
  hibernatedAt?: string;
  miteConnected: boolean;
  miteVersion?: string;
  jenkinsVersion?: string;
  jenkinsHealth?: string;
  lastSeen?: string;
  certExpiry?: string;
  desiredStateHash?: string;
  configHash?: string;
  rbacHash?: string;
  reconciliationPolicy?: ReconciliationPolicy;
  pendingRestart?: PendingRestart;
  pendingItemDeletions?: PendingDeletion[];
  firstConnectedAt?: string;
  lastReconciledAt?: string;
  composedBundleRef?: { name: string; namespace?: string };
  /**
   * The bundle the controller is actually using, resolved server-side.
   * Read-only, and separate from composedBundleRef because that field is PATCHed
   * back when attaching or detaching — resolving into it would silently pin a
   * zero-config controller to the starter bundle on the next save.
   */
  effectiveBundle?: { name: string; namespace: string; builtIn: boolean };
  // Resource override knobs (pre-populate the overlay editor when editing).
  podOverrides?: PodOverrides;
  probes?: ProbesSpec;
  resourceOverlay?: ResourceOverlay;
  observability?: ControllerObservability;
  routingMode?: "subdomain" | "path";
  lastApplyResult?: ApplyResult;
  applyHistory?: ApplyResult[];
  liveDrift?: LiveDrift;
  rollout?: RolloutStatus;
  appliedBundleHash?: string;
  // reconcileBlocked projects the ConditionReconcileBlocked condition (C3).
  // Always present on the detail contract (never nil server-side), so required.
  reconcileBlocked: ReconcileBlocked;
  attention?: ControllerAttention;
  // versionStatus projects the version-roll (A) and upgrade-guard (B) conditions.
  versionStatus?: {
    rollPending?: boolean;
    rollReason?: string;
    rollMessage?: string;
    upgradeBlocked?: boolean;
    blockedReason?: string;
    blockedMessage?: string;
  };
  // miteImageStatus projects the actually-running mite image and whether it
  // is stale vs. the operator-desired image (detect-stale-mite-images, C2).
  miteImageStatus?: {
    image?: string;
    stale?: boolean;
  };
  // pluginConflict projects the ConditionPluginConflict condition (C4) — reused,
  // not a new condition; surfaces the existing plugin-pin-vs-lock check.
  pluginConflict?: { active?: boolean; reason?: string; message?: string };
  // pluginInventory is the bounded plugin inventory summary from Controller.status.
  pluginInventory?: PluginInventorySummary;
  // unappliedRemovals names requested spec removals (explicit nulls) that did
  // not take effect because another field manager still owns the field. Present
  // only on the PATCH response, and only when at least one removal was blocked —
  // an empty or absent array leaves the ordinary success path unchanged.
  unappliedRemovals?: UnappliedRemoval[];
  // Controller conditions from the CRD status (e.g. HibernationCronTriggersSkipped).
  conditions?: Array<{ type: string; status: string; reason?: string; message?: string; lastTransitionTime?: string }>;
}

export interface PluginInventorySummary {
  hash: string;
  collectedAt?: string;
  observedAt?: string;
  source: string;
  stale: boolean;
  degraded: boolean;
  bootstrapApproximate: boolean;
  optionalEdgesDropped: boolean;
  truncated: boolean;
  total: number;
  counts?: Record<string, number>;
  drift?: PluginInventoryDriftEntry[];
  versionDrift?: PluginInventoryDriftEntry[];
  driftTruncated: boolean;
  pendingCollect?: { commandId: string; issuedAt: string } | null;
}

export interface PluginInventoryDriftEntry {
  name: string;
  version?: string;
  class?: string;
  verdict?: string;
  message?: string;
}

// UnappliedRemoval names one requested spec removal (an explicit JSON null)
// that did not take effect: another field manager still owns the field, so
// server-side apply retained it. Surfaced on the PATCH response only, and only
// when at least one removal was blocked.
export interface UnappliedRemoval {
  field: string;
}

export function useController(cluster: string, namespace: string, name: string) {
  return useQuery({
    queryKey: ["controller", cluster, namespace, name],
    queryFn: () =>
      bffFetch<ControllerDetail>(
        `/clusters/${enc(cluster)}/controllers/${enc(namespace)}/${enc(name)}`,
      ),
    refetchInterval: 15_000,
  });
}
