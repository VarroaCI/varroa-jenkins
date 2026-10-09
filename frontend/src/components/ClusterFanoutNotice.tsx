import { useControllerFanoutFailures } from "../hooks/useControllers";
import styles from "./ClusterFanoutNotice.module.css";

/**
 * Names each member cluster whose controllers are absent from the list on
 * this page. Renders nothing when every cluster answered.
 */
export function ClusterFanoutNotice() {
  const failed = useControllerFanoutFailures().data ?? [];
  if (failed.length === 0) return null;
  return (
    <div className={styles.stack} role="status">
      {failed.map((c) => (
        <div key={c.name} className={styles.notice}>
          Controllers of cluster <code>{c.name}</code> are absent from this list: {c.error || "the cluster did not answer"}.
        </div>
      ))}
    </div>
  );
}
