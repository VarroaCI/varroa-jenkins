import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useControllers } from "../hooks/useControllers";
import { deleteController } from "../api/client";
import { BroodControllerPicker } from "../components/BroodControllerPicker";
import { BroodOperationModal } from "../components/BroodOperationModal";
import { Button } from "../components/Button";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere } from "../hooks/usePermissions";
import type { ControllerListItem } from "../hooks/useControllers";
import type { BroodRun } from "../types";
import styles from "./Controllers.module.css";

export default function Controllers() {
  const navigate = useNavigate();
  const { refetch } = useControllers();
  const { permissions } = useAuth();
  const canManage = canDoAnywhere(permissions, "controllers", "manage");

  const [selected, setSelected] = useState<string[]>([]);
  const [broodModalOpen, setBroodModalOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<ControllerListItem | null>(null);
  const [deleting, setDeleting] = useState(false);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteController(deleteTarget.cluster, deleteTarget.name, deleteTarget.namespace);
      setDeleteTarget(null);
      refetch();
    } catch {
      // keep dialog open on failure
    }
    setDeleting(false);
  };

  const handleBroodSubmitted = (result: BroodRun) => {
    setBroodModalOpen(false);
    setSelected([]);
    navigate(`/brood-operations/${result.namespace}/${result.name}`);
  };

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Controllers</div>
          <div className={styles.pageDesc}>
            Each controller is a <span className={styles.mono}>varroa.dev/v1alpha1 · Controller</span> resource
          </div>
        </div>
        {canDoAnywhere(permissions, "controllers", "create") && (
          <Link to="/controllers/create">
            <Button variant="primary">＋ New controller</Button>
          </Link>
        )}
      </div>

      {canManage && selected.length > 0 && (
        <div className={styles.broodBar}>
          <span>{selected.length} controller(s) selected</span>
          <Button onClick={() => setBroodModalOpen(true)}>Run operation…</Button>
          <Button variant="ghost" onClick={() => setSelected([])}>Clear</Button>
        </div>
      )}

      <BroodControllerPicker selected={selected} onSelectionChange={setSelected} />

      {broodModalOpen && (
        <BroodOperationModal
          targets={selected}
          onClose={() => setBroodModalOpen(false)}
          onSubmitted={handleBroodSubmitted}
        />
      )}

      {/* Delete confirmation overlay */}
      {deleteTarget && (
        <div className={styles.overlay} onClick={() => setDeleteTarget(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>Delete Controller</div>
            <p className={styles.dialogBody}>
              Are you sure you want to delete <b>{deleteTarget.name}</b>?
              This action cannot be undone.
            </p>
            <div className={styles.dialogActions}>
              <Button onClick={() => setDeleteTarget(null)}>Cancel</Button>
              <Button variant="primary" onClick={handleDelete} disabled={deleting}>
                {deleting ? "Deleting..." : "Delete"}
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
