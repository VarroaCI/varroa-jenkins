import { useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { useClusters } from "../hooks/useClusters";
import { age } from "../components/activityTimeline.util";
import { drainCluster, cancelClusterDrain } from "../api/client";
import { Card } from "../components/Card";
import type { ClusterEntry } from "../types";
import styles from "./Clusters.module.css";

function statePill(c: ClusterEntry) {
  let cls = styles.stateActive;
  let label = "Active";
  if (c.state === "draining") {
    cls = styles.stateDraining;
    label = "Draining";
  } else if (c.state === "drained") {
    cls = styles.stateDrained;
    label = "Drained";
  }
  return <span className={`${styles.statePill} ${cls}`}>{label}</span>;
}

// Drain confirmation modal.
function DrainModal({
  cluster,
  onClose,
}: {
  cluster: ClusterEntry;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const qc = useQueryClient();

  const handleConfirm = async () => {
    setBusy(true);
    setError(null);
    try {
      await drainCluster(cluster.name, typed);
      qc.invalidateQueries({ queryKey: ["clusters"] });
      onClose();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={styles.overlay} onClick={onClose}>
      <div className={styles.modal} onClick={(e) => e.stopPropagation()}>
        <div className={styles.modalTitle}>Drain cluster "{cluster.name}"</div>
        <div className={styles.modalBody}>
          <p><strong>All controllers on this cluster will be deleted.</strong></p>
          <p>Jenkins data will <strong>not</strong> be migrated.</p>
          <p style={{ marginTop: 12 }}>
            Type <strong>{cluster.name}</strong> to confirm:
          </p>
        </div>
        {error && <div className={styles.modalError}>{error}</div>}
        <input
          className={styles.modalInput}
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          placeholder={cluster.name}
          disabled={busy}
        />
        <div className={styles.modalActions}>
          <button className={styles.modalCancelBtn} onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            className={styles.modalConfirmBtn}
            disabled={typed !== cluster.name || busy}
            onClick={handleConfirm}
          >
            {busy ? "Draining..." : "Confirm Drain"}
          </button>
        </div>
      </div>
    </div>
  );
}

export default function Clusters() {
  const { data, isLoading, error } = useClusters();
  const clusters = data ?? [];
  const [drainingTarget, setDrainingTarget] = useState<ClusterEntry | null>(null);
  const [cancelling, setCancelling] = useState<string | null>(null);
  const qc = useQueryClient();

  // Sort core first, then by name
  const sorted = [...clusters].sort((a, b) => {
    if (a.core) return -1;
    if (b.core) return 1;
    return a.name.localeCompare(b.name);
  });

  const handleCancel = async (name: string) => {
    setCancelling(name);
    try {
      await cancelClusterDrain(name);
      qc.invalidateQueries({ queryKey: ["clusters"] });
    } catch {
      // error swallowed; refetch will surface it
    } finally {
      setCancelling(null);
    }
  };

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Clusters</div>
          <div className={styles.pageDesc}>
            Registered Kubernetes clusters running the Varroa operator
          </div>
        </div>
      </div>

      {drainingTarget && (
        <DrainModal cluster={drainingTarget} onClose={() => setDrainingTarget(null)} />
      )}

      {isLoading && (
        <div className={styles.loadingBanner}>Loading clusters...</div>
      )}
      {error && (
        <div className={styles.errorBanner}>Failed to load: {error.message}</div>
      )}

      {!isLoading && !error && sorted.length === 0 && (
        <div className={styles.empty}>No clusters registered</div>
      )}

      {!isLoading && !error && sorted.length > 0 && (
        <Card>
          <div className={styles.row + " " + styles.head}>
            <span>Cluster</span>
            <span>Health</span>
            <span>State</span>
            <span>Heartbeat</span>
            <span>Operator</span>
            <span>Controllers</span>
            <span />
          </div>
          {sorted.map((c) => (
            <div key={c.name} className={styles.row}>
              <div className={styles.clusterName} data-label="Cluster">
                <span className={styles.nameText}>{c.name}</span>
                {c.core && <span className={styles.coreTag}>core</span>}
              </div>
              <span data-label="Health">
                <span className={`${styles.healthPill} ${c.healthy ? styles.healthy : styles.unhealthy}`}>
                  {c.healthy ? "Healthy" : "Unhealthy"}
                </span>
              </span>
              <span data-label="State">
                {statePill(c)}
                {c.state === "draining" && (
                  <div className={styles.drainMeta}>
                    {c.controllerCount} remaining &middot; {c.drainStartedAt ? age(c.drainStartedAt, { variant: "heartbeat" }) : ""}
                  </div>
                )}
              </span>
              <span className={styles.mono} data-label="Heartbeat">{age(c.lastHeartbeat, { variant: "heartbeat" })}</span>
              <span className={styles.mono} data-label="Operator">{c.operatorVersion}</span>
              <span className={styles.mono} data-label="Controllers">{c.connectedCount}/{c.controllerCount}</span>
              <span style={{ display: "flex", gap: 8, alignItems: "center" }}>
                {!c.core && c.state === "active" && (
                  <button className={styles.drainBtn} onClick={() => setDrainingTarget(c)}>
                    Drain…
                  </button>
                )}
                {!c.core && (c.state === "draining" || c.state === "drained") && (
                  <button
                    className={styles.cancelBtn}
                    onClick={() => handleCancel(c.name)}
                    disabled={cancelling === c.name}
                  >
                    {cancelling === c.name ? "Canceling..." : "Cancel drain"}
                  </button>
                )}
                <Link to={`/controllers?cluster=${encodeURIComponent(c.name)}`} className={styles.drillLink}>
                  View controllers ›
                </Link>
              </span>
            </div>
          ))}
        </Card>
      )}
    </div>
  );
}
