import type { ConflictInfo } from "../api/client";
import styles from "./ConflictDialog.module.css";

interface ConflictDialogProps {
  conflicts: ConflictInfo[];
  onReload: () => void;
  onOverride: () => void;
  open: boolean;
}

export default function ConflictDialog({
  conflicts,
  onReload,
  onOverride,
  open,
}: ConflictDialogProps) {
  if (!open) return null;

  return (
    <div className={styles.overlay} role="dialog" aria-modal="true" aria-label="Field conflict">
      <div className={styles.dialog}>
        <h2 className={styles.heading}>Field ownership conflict</h2>
        <p className={styles.description}>
          Some fields you&apos;re changing were also just changed by another manager:
        </p>

        <div className={styles.conflictList}>
          {conflicts.map((c, i) => (
            <div key={i} className={styles.conflictRow}>
              <div className={styles.conflictField}>{c.field}</div>
              <div className={styles.conflictDetail}>
                {c.manager ? (
                  <>
                    <span className={styles.managerLabel}>Manager:</span>{" "}
                    <code className={styles.managerValue}>{c.manager}</code>
                    <br />
                  </>
                ) : null}
                <span className={styles.messageText}>{c.message}</span>
              </div>
            </div>
          ))}
        </div>

        <div className={styles.actions}>
          <button className={styles.reloadBtn} onClick={onReload} type="button">
            Reload latest
          </button>
          <button className={styles.overrideBtn} onClick={onOverride} type="button">
            Override anyway
          </button>
        </div>
      </div>
    </div>
  );
}
