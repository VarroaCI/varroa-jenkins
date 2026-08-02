import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { getIdentitySettings } from "../api/client";
import { Card } from "../components/Card";
import { KVGrid } from "../components/KVGrid";
import LoadingSpinner from "../components/LoadingSpinner";
import styles from "./settings.module.css";

export default function SettingsIdentityTab() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["identity-settings"],
    queryFn: getIdentitySettings,
  });

  if (isLoading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorMsg}>Failed to load identity settings</div>;
  if (!data) return null;

  const items: { key: string; value: string }[] = [
    { key: "Mode", value: data.mode },
    { key: "Cookie domain", value: data.cookieDomain || "(none)" },
    { key: "Default read access", value: data.defaultRead ? "Yes" : "No" },
  ];

  if (data.mode === "oidc") {
    if (data.issuer) items.push({ key: "Issuer", value: data.issuer });
    if (data.clientId) items.push({ key: "Client ID", value: data.clientId });
    if (data.scopes?.length) items.push({ key: "Scopes", value: data.scopes.join(", ") });
  }

  return (
    <div>
      <p className={`${styles.muted} ${styles.pageNote}`}>
        Identity and OIDC configuration is read-only. These values are set at deploy time via flags or environment variables.
      </p>
      <Card title="Identity Configuration">
        <KVGrid items={items} />
      </Card>
      {data.mode === "local" && (
        <div className={styles.identityNotice}>
          <strong>Local mode</strong> — users and groups are managed in the{" "}
          <Link to="/access/users" className={styles.accentLink}>Users</Link>{" "}
          and{" "}
          <Link to="/access/groups" className={styles.accentLink}>Groups</Link>{" "}
          sections.
        </div>
      )}
    </div>
  );
}
