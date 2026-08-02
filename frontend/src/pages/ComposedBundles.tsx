import { useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useComposedBundles } from "../hooks/useCatalog";
import { deleteComposedBundle } from "../api/client";
import { usePermissions, canDoAnywhere } from "../hooks/usePermissions";
import { clusterQuery } from "../routing";
import { useQueryClient } from "@tanstack/react-query";
import { useToast } from "../components/Toast";
import { Button } from "../components/Button";
import ClusterSelector from "../components/ClusterSelector";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import type { ComposedBundle, ComposedBundlePhase } from "../types";
import styles from "./ComposedBundles.module.css";

function PhasePill({ phase }: { phase?: ComposedBundlePhase }) {
  if (!phase) return null;
  const cls = [
    styles.phasePill,
    phase === "Ready" ? styles.phaseReady : "",
    phase === "Drifted" ? styles.phaseDrifted : "",
    phase === "Invalid" ? styles.phaseInvalid : "",
    phase === "Pending" ? styles.phasePending : "",
  ]
    .filter(Boolean)
    .join(" ");
  return <span className={cls}>{phase}</span>;
}

export default function ComposedBundles() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const { data: perms } = usePermissions();
  const [searchParams, setSearchParams] = useSearchParams();
  const { cluster, ready } = useConfigurationCluster();
  const { data, isLoading, error } = useComposedBundles(cluster);
  const [deleting, setDeleting] = useState<string | null>(null);

  const bundles = data?.items ?? [];

  async function handleDelete(bundle: ComposedBundle) {
    const ns = bundle.metadata.namespace || "default";
    const name = bundle.metadata.name;
    setDeleting(name);
    try {
      await deleteComposedBundle(cluster!, ns, name);
      queryClient.invalidateQueries({ queryKey: ["composed-bundles"] });
      toast("Bundle deleted");
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to delete");
    } finally {
      setDeleting(null);
    }
  }

  function handleRowClick(bundle: ComposedBundle) {
    const ns = bundle.metadata.namespace || "default";
    navigate(
      `/catalog/bundles/${encodeURIComponent(ns)}/${encodeURIComponent(bundle.metadata.name)}${clusterQuery(cluster)}`,
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Composed Bundles</div>
          <div className={styles.pageDesc}>
            Compositions assembled from catalog items
          </div>
        </div>
        {cluster && <ClusterSelector value={cluster} onChange={(value) => {
          const next = new URLSearchParams(searchParams);
          next.set("cluster", value);
          setSearchParams(next);
        }} />}
      </div>

      {ready && !cluster && <NoAccessibleClusters />}
      {error && (
        <div className={styles.errorBanner}>
          Failed to load: {error.message}
        </div>
      )}

      {isLoading && (
        <div className={styles.loadingBanner}>Loading composed bundles...</div>
      )}

      {!isLoading && !error && bundles.length === 0 && (
        <div className={styles.empty}>
          No composed bundles yet. Go to the catalog browser to compose one.
        </div>
      )}

      {!isLoading && !error && bundles.length > 0 && (
        <div className={styles.table}>
          <div className={styles.tableHeader}>
            <span>Name</span>
            <span>Items</span>
            <span>Phase</span>
            <span>Resolved Hash</span>
            <span>Missing</span>
            <span>Drifted</span>
            <span />
          </div>
          {bundles.map((bundle) => {
            const missing = bundle.status?.missingItems ?? [];
            const drifted = bundle.status?.driftedItems ?? [];
            const hash = bundle.status?.resolvedHash ?? "";
            return (
              <div
                key={bundle.metadata.name}
                className={styles.tableRow}
                onClick={() => handleRowClick(bundle)}
              >
                <span className={styles.cellName}>
                  {bundle.spec.displayName || bundle.metadata.name}
                </span>
                <span>{bundle.status?.itemCount ?? bundle.spec.inputs?.length ?? 0}</span>
                <span>
                  <PhasePill phase={bundle.status?.phase} />
                </span>
                <span className={styles.cellMono}>
                  {hash ? hash.substring(0, 12) + "..." : "-"}
                </span>
                <span>
                  {missing.length > 0 ? (
                    <span className={styles.missingBadge}>{missing.length}</span>
                  ) : (
                    <span className={styles.okBadge}>&#x2713;</span>
                  )}
                </span>
                <span>
                  {drifted.length > 0 ? (
                    <span className={styles.driftedBadge}>{drifted.length}</span>
                  ) : (
                    <span className={styles.okBadge}>&#x2713;</span>
                  )}
                </span>
                <div className={styles.actions} onClick={(e) => e.stopPropagation()}>
                  {canDoAnywhere(perms, "composedbundles", "delete") && (
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => handleDelete(bundle)}
                      disabled={deleting === bundle.metadata.name}
                    >
                      {deleting === bundle.metadata.name ? "..." : "Delete"}
                    </Button>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
