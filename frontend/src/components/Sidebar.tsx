import { useState, type FocusEvent, type MouseEvent } from "react";
import { NavLink } from "react-router-dom";
import { Activity, Boxes, ChevronRight, CircleUserRound, Clock3, LayoutDashboard, Network, PanelLeftClose, PanelLeftOpen, PlugZap, Server, Shield, Workflow, type LucideIcon } from "lucide-react";
import { Pulse } from "./Pulse";
import { NavIcon } from "./NavIcon";
import { usePermissions } from "../hooks/usePermissions";
import { canCatalogArea, canAdminArea } from "../lib/navPermissions";
import { useControllers } from "../hooks/useControllers";
import styles from "./Sidebar.module.css";

interface NavItem { to: string; label: string; icon: LucideIcon; end?: boolean; door?: boolean }
interface NavGroup { label: string; items: NavItem[] }

const GROUPS: NavGroup[] = [
  { label: "Operate", items: [
    { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
    { to: "/controllers", label: "Controllers", icon: Server },
    { to: "/plugins", label: "Plugins", icon: PlugZap },
    { to: "/clusters", label: "Clusters", icon: Network },
    { to: "/activity", label: "Activity", icon: Activity },
  ] },
  { label: "Brood", items: [
    { to: "/brood-operations", label: "Operations", icon: Workflow },
    { to: "/brood-schedules", label: "Schedules", icon: Clock3 },
  ] },
  { label: "Manage", items: [
    { to: "/catalog", label: "Catalog", icon: Boxes, door: true },
    { to: "/settings", label: "Admin & access", icon: Shield, door: true },
  ] },
];

function isDoorVisible(item: NavItem, permissions: ReturnType<typeof usePermissions>["data"]): boolean {
  if (!item.door) return true;
  if (item.to === "/catalog") return canCatalogArea(permissions);
  if (item.to === "/settings") return canAdminArea(permissions);
  return true;
}

interface SidebarProps {
  collapsed?: boolean;
  onToggle?: () => void;
}

interface FlyoutState {
  label: string;
  count: number | undefined;
  top: number;
  left: number;
}

export function Sidebar({ collapsed = false, onToggle = () => {} }: SidebarProps) {
  const { data: permissions } = usePermissions();
  const { data: controllers } = useControllers();
  const controllersCount = controllers?.length;
  const [flyout, setFlyout] = useState<FlyoutState | null>(null);

  function handleFlyoutShow(label: string, count: number | undefined) {
    return (e: MouseEvent | FocusEvent) => {
      if (!collapsed) {
        setFlyout(null);
        return;
      }
      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
      setFlyout({
        label,
        count,
        top: rect.top + rect.height / 2,
        left: rect.right + 10,
      });
    };
  }

  function handleFlyoutHide() {
    setFlyout(null);
  }

  return <aside className={`${styles.sidebar} ${collapsed ? styles.collapsed : ""}`}>
    <a href="/" className={styles.brand}><div className={styles.brandMark}><svg viewBox="0 0 34 34" width="34" height="34" aria-hidden="true"><polygon points="17,2 30,9.5 30,24.5 17,32 4,24.5 4,9.5" fill="var(--accent)" /><polygon points="17,9 24,13 24,21 17,25 10,21 10,13" fill="var(--surface)" /><circle cx="17" cy="17" r="2.6" fill="var(--accent)" /></svg></div><div><div className={styles.brandName}>Varroa</div><div className={styles.brandSub}>Jenkins control plane</div></div></a>
    {GROUPS.map((group) => <div key={group.label}><div className={styles.navLabel}>{group.label}</div>{group.items.filter((item) => isDoorVisible(item, permissions)).map((item) => {
      const itemCount = item.to === "/controllers" ? controllersCount : undefined;
      return <NavLink key={item.to} to={item.to} end={item.end} aria-label={itemCount !== undefined ? `${item.label}, ${itemCount}` : item.label} className={({ isActive }) => `${styles.navItem} ${isActive ? styles.active : ""}`} onMouseEnter={handleFlyoutShow(item.label, itemCount)} onMouseLeave={handleFlyoutHide} onFocus={handleFlyoutShow(item.label, itemCount)} onBlur={handleFlyoutHide}><span className={styles.ic}><NavIcon icon={item.icon} /></span><span className={styles.navText}>{item.label}</span>{itemCount !== undefined && <span className={styles.navCount}>{itemCount}</span>}{item.door && <ChevronRight className={styles.doorChevron} aria-hidden={true} size={14} />}</NavLink>;
    })}</div>)}
    <div className={styles.sidebarFoot}>
      <button className={styles.collapseBtn} onClick={onToggle} aria-expanded={!collapsed} aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"} onMouseEnter={handleFlyoutShow(collapsed ? "Expand" : "Collapse", undefined)} onMouseLeave={handleFlyoutHide} onFocus={handleFlyoutShow(collapsed ? "Expand" : "Collapse", undefined)} onBlur={handleFlyoutHide}><span className={styles.ic}><NavIcon icon={collapsed ? PanelLeftOpen : PanelLeftClose} /></span><span className={styles.navText}>{collapsed ? "Expand" : "Collapse"}</span></button>
      <NavLink to="/profile" aria-label="My profile" className={({ isActive }) => `${styles.navItem} ${isActive ? styles.active : ""}`} onMouseEnter={handleFlyoutShow("My profile", undefined)} onMouseLeave={handleFlyoutHide} onFocus={handleFlyoutShow("My profile", undefined)} onBlur={handleFlyoutHide}><span className={styles.ic}><NavIcon icon={CircleUserRound} /></span><span className={styles.navText}>My profile</span></NavLink><div className={styles.healthRow}><Pulse active size={9} /><span className={styles.healthText}>Operator healthy</span></div>
    </div>
    {collapsed && flyout && (
      <div className={styles.flyout} aria-hidden="true" style={{ top: flyout.top, left: flyout.left }}>
        {flyout.label}{flyout.count !== undefined ? ` ${flyout.count}` : ""}
      </div>
    )}
  </aside>;
}
