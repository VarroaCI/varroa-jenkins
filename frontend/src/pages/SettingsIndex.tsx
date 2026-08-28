import { usePermissions } from "../hooks/usePermissions";
import { canAdminArea } from "../lib/navPermissions";
import { SETTINGS_ITEMS, areaItemAllowed } from "../lib/navAreas";
import { NotFoundPage } from "../components/RecoveryState";
import { NavIcon } from "../components/NavIcon";
import { Link } from "react-router-dom";
import styles from "./SettingsIndex.module.css";

export default function SettingsIndex() {
  const { data: permissions } = usePermissions();

  if (!canAdminArea(permissions)) {
    return <NotFoundPage />;
  }

  const visibleItems = SETTINGS_ITEMS.filter((item) => areaItemAllowed(item, permissions));

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Admin & access</div>
          <div className={styles.pageDesc}>Manage users, roles, permissions, provisioning, and identity</div>
        </div>
      </div>
      <div className={styles.grid}>
        {visibleItems.map((item) => (
          <Link key={item.to} to={item.to} className={styles.card} aria-label={item.label}>
            <span className={styles.cardIcon}><NavIcon icon={item.icon} /></span>
            <div className={styles.cardTitle}>{item.label}</div>
            <div className={styles.cardDesc}>{item.description}</div>
          </Link>
        ))}
      </div>
    </div>
  );
}
