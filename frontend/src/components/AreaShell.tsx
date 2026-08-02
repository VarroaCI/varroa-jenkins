import { NavLink, Outlet } from "react-router-dom";
import { NavIcon } from "./NavIcon";
import { usePermissions } from "../hooks/usePermissions";
import { areaItemAllowed, type AreaNavItem } from "../lib/navAreas";
import styles from "./AreaShell.module.css";

interface AreaShellProps {
  items: AreaNavItem[];
  title: string;
}

export function AreaShell({ items, title }: AreaShellProps) {
  const { data: permissions } = usePermissions();
  const visible = items.filter((item) => areaItemAllowed(item, permissions));

  return (
    <div className={styles.shell}>
      <nav className={styles.subNav} aria-label={title}>
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
