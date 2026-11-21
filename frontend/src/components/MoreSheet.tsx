import { Fragment, useEffect, useRef, useCallback } from "react";
import { NavLink } from "react-router-dom";
import { NavIcon } from "./NavIcon";
import { usePermissions } from "../hooks/usePermissions";
import { NAV_GROUPS, PROFILE_ROW, visibleNavGroups, type NavRowGroup } from "../lib/navRows";
import styles from "./MoreSheet.module.css";

// The sheet carries every table row the tab bar does not, then the Account group.
const SHEET_GROUPS: readonly NavRowGroup[] = [
  ...NAV_GROUPS.map((group) => ({ label: group.label, rows: group.rows.filter((row) => !row.tab) })),
  { label: "Account", rows: [PROFILE_ROW] },
];

interface MoreSheetProps {
  onClose: () => void;
}

export function MoreSheet({ onClose }: MoreSheetProps) {
  const { data: permissions } = usePermissions();
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
        {visibleNavGroups(SHEET_GROUPS, permissions).map((group) => (
          <Fragment key={group.label}>
            <div className={styles.groupLabel}>{group.label}</div>
            {group.rows.map((row) => (
              <NavLink key={row.to} to={row.to} end={row.end} className={({ isActive }) => `${styles.row} ${isActive ? styles.active : ""}`} aria-label={row.label} onClick={handleNav}>
                <span className={styles.rowIcon}><NavIcon icon={row.icon} /></span>
                <span className={styles.rowText}>{row.label}</span>
              </NavLink>
            ))}
          </Fragment>
        ))}
      </div>
    </>
  );
}
