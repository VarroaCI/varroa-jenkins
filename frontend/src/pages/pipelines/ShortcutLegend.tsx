import { useEffect, useRef } from "react";
import styles from "./ShortcutLegend.module.css";

/** The keys the build page binds. Each row names one key and its action. */
export const BUILD_PAGE_SHORTCUTS = [
  { key: "j", action: "Open the next older build" },
  { key: "k", action: "Open the next newer build" },
  { key: "l", action: "Move focus to the log viewer" },
  { key: "/", action: "Focus the log search box" },
  { key: "f", action: "Toggle follow" },
  { key: "t", action: "Toggle timestamps" },
  { key: "?", action: "Show or hide this list" },
] as const;

/**
 * ShortcutLegend lists the build page keys in a small dialog. The dialog is
 * not modal, so the page does not trap focus. While it is open, the build
 * page keys do not act; only ? and Escape act on it. The close button takes
 * focus when the list opens.
 */
export function ShortcutLegend({ open, onClose }: { open: boolean; onClose: () => void }) {
  const closeRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (open) closeRef.current?.focus();
  }, [open]);

  if (!open) return null;

  return (
    <div role="dialog" aria-modal="false" aria-labelledby="build-shortcuts-heading" className={styles.legend}>
      <h2 id="build-shortcuts-heading" className={styles.heading}>
        Keyboard shortcuts
      </h2>
      <dl className={styles.list}>
        {BUILD_PAGE_SHORTCUTS.map((s) => (
          <div key={s.key} className={styles.row}>
            <dt>
              <kbd className={styles.key}>{s.key}</kbd>
            </dt>
            <dd className={styles.action}>{s.action}</dd>
          </div>
        ))}
      </dl>
      <button ref={closeRef} type="button" className={styles.close} onClick={onClose}>
        Close
      </button>
    </div>
  );
}
