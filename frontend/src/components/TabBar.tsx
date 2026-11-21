import { useState, useRef, useCallback } from "react";
import { NavLink } from "react-router-dom";
import { MoreHorizontal } from "lucide-react";
import { NavIcon } from "./NavIcon";
import { useControllers } from "../hooks/useControllers";
import { usePermissions } from "../hooks/usePermissions";
import { canControllersRow } from "../lib/navPermissions";
import { TAB_ROWS, navRowVisible } from "../lib/navRows";
import { MoreSheet } from "./MoreSheet";
import styles from "./TabBar.module.css";

export function TabBar() {
  const { data: permissions } = usePermissions();
  const showControllers = canControllersRow(permissions);
  const { data: controllers } = useControllers(showControllers);
  const controllersCount = controllers?.length;
  const [moreOpen, setMoreOpen] = useState(false);
  const moreBtnRef = useRef<HTMLButtonElement>(null);

  const handleClose = useCallback(() => {
    setMoreOpen(false);
    // Focus must return to the More button after sheet closes
    moreBtnRef.current?.focus();
  }, []);

  return (
    <>
      <nav className={styles.tabBar} aria-label="Primary">
        {TAB_ROWS.filter((row) => navRowVisible(row, permissions)).map((row) => (
          <NavLink key={row.to} to={row.to} end={row.end} aria-label={row.label} className={({ isActive }) => `${styles.tab} ${isActive ? styles.active : ""}`}>
            <span className={styles.tabIcon}><NavIcon icon={row.icon} /></span>
            <span className={styles.tabLabel}>{row.label}</span>
            {row.count === "controllers" && controllersCount !== undefined && <span className={styles.tabCount}>{controllersCount}</span>}
          </NavLink>
        ))}
        <button
          ref={moreBtnRef}
          className={styles.tab}
          aria-expanded={moreOpen}
          aria-haspopup="dialog"
          onClick={() => setMoreOpen((o) => !o)}
        >
          <span className={styles.tabIcon}><NavIcon icon={MoreHorizontal} /></span>
          <span className={styles.tabLabel}>More</span>
        </button>
      </nav>
      {moreOpen && <MoreSheet onClose={handleClose} />}
    </>
  );
}
