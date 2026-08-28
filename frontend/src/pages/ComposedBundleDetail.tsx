import { useEffect, useState } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { getComposedBundle, deleteComposedBundle, updateController } from "../api/client";
import { usePermissions, canDoAnywhere } from "../hooks/usePermissions";
import { clusterQuery, controllerRoute } from "../routing";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import { useControllers } from "../hooks/useControllers";
import type { ControllerListItem } from "../hooks/useControllers";
import { useQueryClient, useMutation } from "@tanstack/react-query";
import { useToast } from "../components/Toast";
import { Button } from "../components/Button";
import { Pulse } from "../components/Pulse";
import { BundleHealthBadge } from "../components/BundleSelector";
import { pauseBundleRollout, resumeBundleRollout } from "../api/client";
import type { ComposedBundle, ComposedItemRef } from "../types";
import styles from "./ComposedBundleDetail.module.css";

/** Extract displayable items from spec.inputs (unified model). */
function specItems(spec: ComposedBundle["spec"]): ComposedItemRef[] {
  if (!spec.inputs) return [];
  return spec.inputs.map((inp) => {
    if (inp.itemRef) return inp.itemRef;
    if (inp.gitSource) {
      return {
        name: `${inp.gitSource.repoURL}#${inp.gitSource.path}`,
        pinnedContentHash: inp.gitSource.revision,
      };
    }
    return { name: "<unknown input>" };
  });
}

function onTargetCount(ctrls: { appliedBundleHash?: string }[], target?: string): number {
  if (!target) return 0;
  return ctrls.filter((c) => c.appliedBundleHash === target).length;
}

function groupByWave(ctrls: ControllerListItem[]): [number, ControllerListItem[]][] {
  const groups = new Map<number, typeof ctrls>();
  for (const c of ctrls) {
    const wave = c.rolloutWave ?? 0;
    if (!groups.has(wave)) groups.set(wave, []);
    groups.get(wave)!.push(c);
  }
  const sorted = Array.from(groups.entries()).sort(([a], [b]) => a - b);
  return sorted;
}

export default function ComposedBundleDetail() {
  const { namespace = "", name = "" } = useParams();
  const { cluster, ready } = useConfigurationCluster();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const { data: perms } = usePermissions();
  const { data: controllers } = useControllers();

  const [bundle, setBundle] = useState<ComposedBundle | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  // Attach to controller state
  const [selectedController, setSelectedController] = useState("");

  useEffect(() => {
    if (!cluster || !name || !namespace) return;
    setLoading(true);
    setError(null);
    getComposedBundle(cluster, namespace, name)
      .then(setBundle)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [cluster, name, namespace]);

  // Attachment choices and impact are scoped to the bundle's full identity.
  const namespaceControllers = controllers?.filter(
    (c) => c.cluster === cluster && c.namespace === (bundle?.metadata.namespace || "default")
  ) ?? [];

  // Controllers using this bundle. Matched on the EFFECTIVE bundle, not the raw
  // spec ref: a controller that names no bundle runs the built-in starter, and
  // filtering on composedBundleRef alone omitted every such controller from the
  // starter bundle's own consumer list.
  const referencingControllers = controllers?.filter((c) => {
    if (c.cluster !== cluster) return false;
    const targetNS = bundle?.metadata.namespace || namespace;
    if (c.effectiveBundle) {
      return c.effectiveBundle.name === name && c.effectiveBundle.namespace === targetNS;
    }
    const ref = c.composedBundleRef;
    if (!ref || ref.name !== name) return false;
    return (ref.namespace || c.namespace) === targetNS;
  }) ?? [];

  const attachMutation = useMutation({
    mutationFn: async () => {
      const ctrl = namespaceControllers.find((c) => c.name === selectedController);
      if (!ctrl) throw new Error("Controller not found");
      return updateController(ctrl.cluster, ctrl.name, ctrl.namespace, {
        spec: { composedBundleRef: { name: bundle!.metadata.name } },
      });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["controllers"] });
      queryClient.invalidateQueries({ queryKey: ["composed-bundles"] });
      toast("Bundle attached to controller");
      setSelectedController("");
    },
    onError: (e: Error) => {
      toast(`Failed to attach: ${e.message}`);
    },
  });

  const isPaused = bundle?.metadata?.annotations?.["varroa.dev/rollout-paused"] === "true";

  const pauseMutation = useMutation({
    mutationFn: () => pauseBundleRollout(cluster!, namespace, name),
    onSuccess: () => {
      toast("Rollout paused");
      getComposedBundle(cluster!, namespace, name).then(setBundle);
      queryClient.invalidateQueries();
    },
    onError: (e: Error) => toast(`Pause failed: ${e.message}`),
  });

  const resumeMutation = useMutation({
    mutationFn: () => resumeBundleRollout(cluster!, namespace, name),
    onSuccess: () => {
      toast("Rollout resumed");
      getComposedBundle(cluster!, namespace, name).then(setBundle);
      queryClient.invalidateQueries();
    },
    onError: (e: Error) => toast(`Resume failed: ${e.message}`),
  });

  async function handleDelete() {
    if (!bundle) return;
    setDeleting(true);
    try {
      await deleteComposedBundle(cluster!, namespace, name);
      queryClient.invalidateQueries({ queryKey: ["composed-bundles"] });
      toast("Bundle deleted");
      navigate(`/catalog/bundles${clusterQuery(cluster)}`);
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to delete");
    } finally {
      setDeleting(false);
    }
  }

  function handleEdit() {
    if (!bundle) return;
    navigate(`/catalog/bundles/${namespace}/${name}/edit${clusterQuery(cluster)}`);
  }

  if (ready && !cluster) return <NoAccessibleClusters />;
  if (loading) {
    return (
      <div className={styles.page}>
        <div className={styles.loadingBanner}>Loading bundle...</div>
      </div>
    );
  }

  if (error || !bundle) {
    return (
      <div className={styles.page}>
        <Link to="/catalog/bundles" className={styles.backLink}>
          &larr; Back to Bundles
        </Link>
        <div className={styles.errorBanner}>
          {error ? `Failed to load: ${error}` : "Bundle not found"}
        </div>
      </div>
    );
  }

  const spec = bundle.spec;
  const status = bundle.status;
  const missing = status?.missingItems ?? [];
  const drifted = status?.driftedItems ?? [];

  return (
    <div className={styles.page}>
      <Link to={`/catalog/bundles${clusterQuery(cluster)}`} className={styles.backLink}>
        &larr; Back to Bundles
      </Link>

      <div className={styles.headRow}>
        <div>
          <h2 className={styles.title}>{spec.displayName || bundle.metadata.name}</h2>
          <p className={styles.subtitle}>
            Composed bundle &middot; {bundle.metadata.namespace || "default"}
          </p>
        </div>
        <div className={styles.headActions}>
          {canDoAnywhere(perms, "composedbundles", "update") && (
            <Button variant="primary" onClick={handleEdit}>
              Edit in composer
            </Button>
          )}
          {canDoAnywhere(perms, "composedbundles", "delete") && (
            <Button variant="ghost" onClick={handleDelete} disabled={deleting}>
              {deleting ? "Deleting..." : "Delete"}
            </Button>
          )}
        </div>
      </div>

      <div className={styles.phaseRow}>
        <BundleHealthBadge phase={status?.phase} />
      </div>

      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>Summary</h3>
        <div className={styles.detailGrid}>
          <span className={styles.detailLabel}>Name</span>
          <span>{bundle.metadata.name}</span>
          <span className={styles.detailLabel}>Namespace</span>
          <span>{bundle.metadata.namespace || "default"}</span>
          <span className={styles.detailLabel}>Cluster</span>
          <span>{cluster}</span>
          <span className={styles.detailLabel}>Phase</span>
          <span>{status?.phase || "Pending"}</span>
          <span className={styles.detailLabel}>Item count</span>
          <span>{status?.itemCount ?? specItems(spec).length}</span>
          <span className={styles.detailLabel}>JCasC strategy</span>
          <span>{spec.jcascMergeStrategy || "merge"}</span>
          {spec.description && (
            <>
              <span className={styles.detailLabel}>Description</span>
              <span>{spec.description}</span>
            </>
          )}
          {status?.resolvedHash && (
            <>
              <span className={styles.detailLabel}>Resolved hash</span>
              <span className={styles.mono}>{status.resolvedHash}</span>
            </>
          )}
          <span className={styles.detailLabel}>Resource version</span>
          <span className={styles.mono}>{bundle.metadata.resourceVersion || "-"}</span>
          <span className={styles.detailLabel}>Created</span>
          <span>
            {bundle.metadata.creationTimestamp
              ? new Date(bundle.metadata.creationTimestamp).toLocaleDateString()
              : "-"}
          </span>
        </div>
      </div>

      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>Validation</h3>
        <p className={styles.validationLead}>
          {status?.phase === "Ready" ? "Bundle inputs are resolved and ready." : status?.message || "Bundle validation is pending."}
        </p>
        {status?.phase === "Invalid" && (
          <div className={styles.missingBanner}><strong>Bundle invalid.</strong> Review the failures below, correct the bundle inputs, then save to validate again.</div>
        )}
        {missing.length > 0 && <div className={styles.missingBanner}><strong>Missing items:</strong> {missing.join(", ")}. Restore or replace these inputs.</div>}
        {drifted.length > 0 && <div className={styles.driftedBanner}><strong>Drifted items:</strong> {drifted.join(", ")}. Review and update pinned revisions.</div>}
        {(status?.errors ?? []).map((error, index) => <div className={styles.missingBanner} key={index}>{error}</div>)}
        {(status?.warnings ?? []).length > 0 && (
          <div className={styles.warningsBanner}><strong>Warnings:</strong><ul>{status!.warnings!.map((warning, index) => <li key={index}>{warning}</li>)}</ul></div>
        )}
        {(status?.conditions ?? []).map((condition) => (
          <div className={styles.condition} key={condition.type}>
            <strong>{condition.type}</strong>: {condition.status}
            {condition.reason && <> ({condition.reason})</>}
            {condition.message && <div>{condition.message}</div>}
          </div>
        ))}
      </div>

      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>Composition</h3>
        {(bundle.resolvedInputs ?? []).length === 0 ? <p className={styles.emptyText}>No inputs in this bundle.</p> : (
          <div className={styles.itemList}>
            {bundle.resolvedInputs!.map((input) => (
              <div className={styles.itemRow} key={input.index}>
                <div><div className={styles.itemName}>{input.index + 1}. {input.name}</div><div className={styles.itemMeta}>{input.kind} · {input.type || "unknown type"} · {input.namespace || "bundle namespace"}{input.revision ? ` · ${input.revision}` : ""}</div></div>
                <span className={input.status === "Missing" ? styles.itemMissingBadge : input.status === "Drifted" ? styles.itemDriftedBadge : styles.statusBadge}>{input.status}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Attach to controller */}
      {canDoAnywhere(perms, "controllers", "update") && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Impact</h3>
          <h4>Attach to controller</h4>
          <div className={styles.attachRow}>
            <select
              className={styles.formSelect}
              value={selectedController}
              onChange={(e) => setSelectedController(e.target.value)}
            >
              <option value="">-- Select controller --</option>
              {namespaceControllers.map((c) => (
                <option key={c.name} value={c.name}>
                  {c.name}
                </option>
              ))}
            </select>
            <Button
              variant="primary"
              disabled={!selectedController || attachMutation.isPending}
              onClick={() => attachMutation.mutate()}
            >
              {attachMutation.isPending ? "Attaching..." : "Attach"}
            </Button>
          </div>
          {namespaceControllers.length === 0 && (
            <p className={styles.emptyText}>
              No controllers found in namespace "{bundle.metadata.namespace || "default"}"
            </p>
          )}
        </div>
      )}

      {/* Referenced by controllers — fan-out view */}
      <div className={styles.section}>
        <div className={styles.sectionHead}>
          <h3 className={styles.sectionTitle}>
            Impact: Controllers ({referencingControllers.length})
          </h3>
          {/* Pause / Resume */}
          {canDoAnywhere(perms, "composedbundles", "update") && (
            <div className={styles.pauseControls}>
              {isPaused ? (
                <Button variant="primary" size="sm" onClick={() => resumeMutation.mutate()} disabled={resumeMutation.isPending}>
                  {resumeMutation.isPending ? "Resuming..." : "▶ Resume rollout"}
                </Button>
              ) : (
                <Button variant="ghost" size="sm" onClick={() => pauseMutation.mutate()} disabled={pauseMutation.isPending}>
                  {pauseMutation.isPending ? "Pausing..." : "⏸ Pause rollout"}
                </Button>
              )}
            </div>
          )}
        </div>

        {/* Rollup summary */}
        {referencingControllers.length > 0 && (
          <p className={styles.rollupSummary}>
            {status?.resolvedHash
              ? `${onTargetCount(referencingControllers, status.resolvedHash)} of ${referencingControllers.length} on latest`
              : "target not resolved"}
            {isPaused && <span className={styles.pausedTag}> · rollout paused</span>}
          </p>
        )}

        {/* Wave-grouped controller list */}
        {referencingControllers.length === 0 ? (
          <p className={styles.emptyText}>No controllers reference this bundle.</p>
        ) : (
          <div className={styles.waveList}>
            {groupByWave(referencingControllers).map(([wave, ctrls]) => {
              const onTarget = status?.resolvedHash
                ? ctrls.filter((c) => c.appliedBundleHash === status.resolvedHash).length
                : 0;
              return (
                <div key={wave} className={styles.waveGroup}>
                  <div className={styles.waveLabel}>
                    Wave {wave} · {onTarget}/{ctrls.length} on target
                  </div>
                  {ctrls.map((c) => {
                    const isOnTarget = status?.resolvedHash && c.appliedBundleHash === status.resolvedHash;
                    return (
                      <Link
                        key={c.name}
                        to={controllerRoute(c.cluster, c.namespace, c.name)}
                        className={styles.refLink}
                      >
                        <span className={isOnTarget ? styles.refOnTarget : styles.refPending}>
                          {isOnTarget ? "✓" : "○"}
                        </span>
                        <span className={styles.refName}>{c.name}</span>
                        <span className={styles.refNamespace}>{c.namespace}</span>
                        <span className={styles.refPhase}>{c.phase}</span>
                        <Pulse active={c.miteConnected} size={6} />
                      </Link>
                    );
                  })}
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* Items */}
      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>
          Items ({specItems(spec).length})
        </h3>
        {specItems(spec).length === 0 ? (
          <p className={styles.emptyText}>No items in this bundle.</p>
        ) : (
          <div className={styles.itemList}>
            {specItems(spec).map((item) => (
              <div key={item.namespace ? `${item.namespace}/${item.name}` : item.name} className={styles.itemRow}>
                <div>
                  <div className={styles.itemName}>
                    {item.name}
                    {item.namespace && (
                      <span className={styles.nsBadge}>{item.namespace}</span>
                    )}
                  </div>
                  {item.pinnedContentHash && (
                    <div className={styles.itemMeta}>
                      Pinned: {item.pinnedContentHash.substring(0, 12)}...
                    </div>
                  )}
                </div>
                {missing.includes(item.name) && (
                  <span className={styles.itemMissingBadge}>Missing</span>
                )}
                {drifted.includes(item.name) && (
                  <span className={styles.itemDriftedBadge}>Drifted</span>
                )}
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Variables */}
      {spec.variables && Object.keys(spec.variables).length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>
            Variables ({Object.keys(spec.variables).length})
          </h3>
          <div className={styles.varGrid}>
            {Object.entries(spec.variables).map(([k, v]) => (
              <div key={k} className={styles.varRow}>
                <span className={styles.varKey}>{k}</span>
                <span className={styles.varValue}>{v}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      <details className={styles.diagnostics}>
        <summary>Diagnostics</summary>
        <h4>Metadata</h4>
        <pre>{JSON.stringify(bundle.metadata, null, 2)}</pre>
        <h4>Conditions</h4>
        <pre>{JSON.stringify(status?.conditions ?? [], null, 2)}</pre>
      </details>
    </div>
  );
}
