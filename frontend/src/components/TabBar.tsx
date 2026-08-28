import { useState, useRef, useCallback } from "react";
import { NavLink } from "react-router-dom";
import { Activity, LayoutDashboard, MoreHorizontal, Network, Server } from "lucide-react";
import { NavIcon } from "./NavIcon";
import { useControllers } from "../hooks/useControllers";
import { MoreSheet } from "./MoreSheet";
import styles from "./TabBar.module.css";

export function TabBar() {
  const { data: controllers } = useControllers();
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
        <NavLink to="/" end aria-label="Dashboard" className={({ isActive }) => `${styles.tab} ${isActive ? styles.active : ""}`}>
          <span className={styles.tabIcon}><NavIcon icon={LayoutDashboard} /></span>
          <span className={styles.tabLabel}>Dashboard</span>
        </NavLink>
        <NavLink to="/controllers" aria-label="Controllers" className={({ isActive }) => `${styles.tab} ${isActive ? styles.active : ""}`}>
          <span className={styles.tabIcon}><NavIcon icon={Server} /></span>
          <span className={styles.tabLabel}>Controllers</span>
          {controllersCount !== undefined && <span className={styles.tabCount}>{controllersCount}</span>}
        </NavLink>
        <NavLink to="/clusters" aria-label="Clusters" className={({ isActive }) => `${styles.tab} ${isActive ? styles.active : ""}`}>
          <span className={styles.tabIcon}><NavIcon icon={Network} /></span>
          <span className={styles.tabLabel}>Clusters</span>
        </NavLink>
        <NavLink to="/activity" aria-label="Activity" className={({ isActive }) => `${styles.tab} ${isActive ? styles.active : ""}`}>
          <span className={styles.tabIcon}><NavIcon icon={Activity} /></span>
          <span className={styles.tabLabel}>Activity</span>
        </NavLink>
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
