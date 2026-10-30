import { useEffect, useRef } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { NavIcon } from "./NavIcon";
import { usePermissions } from "../hooks/usePermissions";
import { useNarrowViewport } from "../hooks/useNarrowViewport";
import { areaItemAllowed, type AreaNavItem } from "../lib/navAreas";
import styles from "./AreaShell.module.css";

interface AreaShellProps {
  items: AreaNavItem[];
  title: string;
}

export function AreaShell({ items, title }: AreaShellProps) {
  const { data: permissions } = usePermissions();
  const visible = items.filter((item) => areaItemAllowed(item, permissions));
  const navRef = useRef<HTMLElement>(null);
  const { pathname } = useLocation();
  const narrow = useNarrowViewport();

  // The narrow strip scrolls sideways. Center the active item so it stays visible.
  // Re-run when the viewport crosses the breakpoint, so a resize keeps it in view.
  useEffect(() => {
    const nav = navRef.current;
    if (!nav || nav.scrollWidth <= nav.clientWidth) return;
    const active = nav.querySelector<HTMLElement>('[aria-current="page"]');
    if (!active) return;
    nav.scrollLeft = Math.max(0, active.offsetLeft - (nav.clientWidth - active.offsetWidth) / 2);
  }, [pathname, visible.length, narrow]);

  return (
    <div className={styles.shell}>
      <nav ref={navRef} className={styles.subNav} aria-label={title}>
        {visible.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={({ isActive }) =>
              `${styles.navItem} ${isActive ? styles.active : ""}`
            }
          >
            <span className={styles.ic}>
              <NavIcon icon={item.icon} />
            </span>
            <span className={styles.navText}>{item.label}</span>
          </NavLink>
        ))}
      </nav>
      <div className={styles.content}>
        <Outlet />
      </div>
    </div>
  );
}
