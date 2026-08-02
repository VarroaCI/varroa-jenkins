import { useState } from "react";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import { Card } from "../components/Card";
import { Tabs } from "../components/Tabs";
import SettingsProvisioningTab from "./SettingsProvisioningTab";
import SettingsVersionsTab from "./SettingsVersionsTab";
import SettingsIdentityTab from "./SettingsIdentityTab";
import SettingsBuiltinRolesTab from "./SettingsBuiltinRolesTab";
import SettingsUsersTab from "./SettingsUsersTab";
import SettingsGroupsTab from "./SettingsGroupsTab";
import styles from "./settings.module.css";

type TabId = "provisioning" | "versions" | "identity" | "users" | "groups" | "builtin-roles";

interface Tab {
  id: TabId;
  label: string;
}

export default function Settings() {
  const { permissions } = useAuth();
  // Admin sections are gated strictly on varroa:admin (wildcard */*). Note this
  // is deliberately NOT controllers:create, which the operator role also has.
  const isAdmin = canDoGlobal(permissions, "*", "*");

  const tabs: Tab[] = [
    ...(canDoGlobal(permissions, "provisioningdefaults", "update") ? [
      { id: "provisioning" as TabId, label: "Provisioning" },
      { id: "versions" as TabId, label: "Versions" },
    ] : []),
    ...(isAdmin ? [
      { id: "identity" as TabId, label: "Identity" },
      { id: "users" as TabId, label: "Users" },
      { id: "groups" as TabId, label: "Groups" },
      { id: "builtin-roles" as TabId, label: "Built-in Roles" },
    ] : []),
  ];

  const [active, setActive] = useState<TabId>(tabs[0]?.id || "provisioning");

  if (tabs.length === 0) {
    return (
      <div className={styles.page}>
        <div className={styles.pageHead}>
          <h1 className={styles.pageTitle}>Settings</h1>
        </div>
        <Card>
          <p className={styles.muted}>You don't have permission to access any settings sections.</p>
        </Card>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <h1 className={styles.pageTitle}>Settings</h1>
          <p className={styles.pageDesc}>Administer Varroa configuration</p>
        </div>
      </div>

      <Tabs tabs={tabs} activeTab={active} onSelect={(id) => setActive(id as TabId)} />

      <Card>
        {active === "provisioning" && <SettingsProvisioningTab />}
        {active === "versions" && <SettingsVersionsTab />}
        {active === "identity" && <SettingsIdentityTab />}
        {active === "users" && <SettingsUsersTab />}
        {active === "groups" && <SettingsGroupsTab />}
        {active === "builtin-roles" && <SettingsBuiltinRolesTab />}
      </Card>
    </div>
  );
}
