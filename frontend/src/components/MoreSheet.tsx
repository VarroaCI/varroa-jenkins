import { useEffect, useRef, useCallback } from "react";
import { NavLink } from "react-router-dom";
import { Boxes, CircleUserRound, Clock3, PlugZap, Shield, Workflow } from "lucide-react";
import { NavIcon } from "./NavIcon";
import { usePermissions } from "../hooks/usePermissions";
import { canCatalogArea, canAdminArea } from "../lib/navPermissions";
import { canDoAnywhere } from "../hooks/usePermissions";
import styles from "./MoreSheet.module.css";

interface MoreSheetProps {
  onClose: () => void;
}

export function MoreSheet({ onClose }: MoreSheetProps) {
  const { data: permissions } = usePermissions();
  const showOperate = canDoAnywhere(permissions, "controllers", "read");
  const showCatalog = canCatalogArea(permissions);
  const showAdmin = canAdminArea(permissions);
  const showManage = showCatalog || showAdmin;
  const sheetRef = useRef<HTMLDivElement>(null);

  const handleNav = useCallback(() => {
    onClose();
  }, [onClose]);

  // Focus first link on mount, body scroll lock, keydown trap
  useEffect(() => {
    const sheet = sheetRef.current;
    if (sheet) {
      const links = sheet.querySelectorAll<HTMLAnchorElement>("a");
      if (links.length > 0) {
        links[0].focus();
      }
    }

    const savedOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
        return;
      }
      if (e.key === "Tab" && sheet) {
        const links = Array.from(sheet.querySelectorAll<HTMLAnchorElement>("a"));
        if (links.length === 0) return;
        const first = links[0];
        const last = links[links.length - 1];
        if (e.shiftKey) {
          if (document.activeElement === first) {
            e.preventDefault();
            last.focus();
          }
        } else {
          if (document.activeElement === last) {
            e.preventDefault();
            first.focus();
          }
        }
      }
    };

    document.addEventListener("keydown", handleKeyDown);

    return () => {
      document.body.style.overflow = savedOverflow;
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [onClose]);

  return (
    <>
      <div className={styles.scrim} onClick={onClose} />
      <div className={styles.sheet} role="dialog" aria-modal="true" aria-label="More navigation" ref={sheetRef}>
        {/* Operate — hidden entirely when gate fails */}
        {showOperate && (
          <>
            <div className={styles.groupLabel}>Operate</div>
            <NavLink to="/plugins" className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label="Plugins" onClick={handleNav}>
              <span className={styles.rowIcon}><NavIcon icon={PlugZap} /></span>
              <span className={styles.rowText}>Plugins</span>
            </NavLink>
          </>
        )}

        {/* Brood */}
        <div className={styles.groupLabel}>Brood</div>
        <NavLink to="/brood-operations" className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label="Operations" onClick={handleNav}>
          <span className={styles.rowIcon}><NavIcon icon={Workflow} /></span>
          <span className={styles.rowText}>Operations</span>
        </NavLink>
        <NavLink to="/brood-schedules" className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label="Schedules" onClick={handleNav}>
          <span className={styles.rowIcon}><NavIcon icon={Clock3} /></span>
          <span className={styles.rowText}>Schedules</span>
        </NavLink>

        {/* Manage — hidden if neither door is visible */}
        {showManage && (
          <>
            <div className={styles.groupLabel}>Manage</div>
            {showCatalog && (
              <NavLink to="/catalog" className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label="Catalog" onClick={handleNav}>
                <span className={styles.rowIcon}><NavIcon icon={Boxes} /></span>
                <span className={styles.rowText}>Catalog</span>
              </NavLink>
            )}
            {showAdmin && (
              <NavLink to="/settings" className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label="Admin & access" onClick={handleNav}>
                <span className={styles.rowIcon}><NavIcon icon={Shield} /></span>
                <span className={styles.rowText}>Admin & access</span>
              </NavLink>
            )}
          </>
        )}

        {/* Account */}
        <div className={styles.groupLabel}>Account</div>
        <NavLink to="/profile" className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label="My profile" onClick={handleNav}>
          <span className={styles.rowIcon}><NavIcon icon={CircleUserRound} /></span>
          <span className={styles.rowText}>My profile</span>
        </NavLink>
      </div>
    </>
  );
}
