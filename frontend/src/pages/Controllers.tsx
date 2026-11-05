import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { BroodControllerPicker } from "../components/BroodControllerPicker";
import { ClusterFanoutNotice } from "../components/ClusterFanoutNotice";
import { BroodOperationModal } from "../components/BroodOperationModal";
import { Button } from "../components/Button";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere } from "../hooks/usePermissions";
import type { BroodRun } from "../types";
import styles from "./Controllers.module.css";

export default function Controllers() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const canManage = canDoAnywhere(permissions, "controllers", "manage");

  const [selected, setSelected] = useState<string[]>([]);
  const [broodModalOpen, setBroodModalOpen] = useState(false);

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

      <ClusterFanoutNotice />
      <BroodControllerPicker selected={selected} onSelectionChange={setSelected} />

      {broodModalOpen && (
        <BroodOperationModal
          targets={selected}
          onClose={() => setBroodModalOpen(false)}
          onSubmitted={handleBroodSubmitted}
        />
      )}
    </div>
  );
}
