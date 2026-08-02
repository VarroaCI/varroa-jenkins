import { useQuery } from "@tanstack/react-query";
import { getBuiltinRoles } from "../api/client";
import { Card } from "../components/Card";
import LoadingSpinner from "../components/LoadingSpinner";
import styles from "./settings.module.css";

export default function SettingsBuiltinRolesTab() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["builtin-roles"],
    queryFn: getBuiltinRoles,
  });

  if (isLoading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorMsg}>Failed to load built-in roles</div>;
  if (!data?.length) return <p className={styles.muted}>No built-in roles found.</p>;

  return (
    <div>
      <p className={`${styles.muted} ${styles.pageNote}`}>
        Built-in roles are sourced live from VarroaRole CRDs. These are read-only as part of the system.
      </p>
      {data.map((role) => (
        <div key={role.name} className={styles.stack16}>
        <Card title={role.name}>
          <div className={styles.gridTwo}>
            <div>
              <h4 className={styles.roleSectionTitle}>
                Control-plane API Rules
              </h4>
              {role.apiRules.map((rule, i) => (
                <div key={i} className={styles.roleRule}>
                  <div className={styles.roleVerbs}>
                    {rule.verbs.join(", ")}
                  </div>
                  <div className={styles.roleDetail}>
                    {rule.resources.join(", ")}
                  </div>
                </div>
              ))}
            </div>
            <div>
              <h4 className={styles.roleSectionTitle}>
                Data-plane Jenkins Role
              </h4>
              {role.jenkinsRoleRef ? (
                <>
                  <div className={styles.stack12}>
                    <a className={styles.accentLink} href={`/access/jenkins-roles/${encodeURIComponent(role.jenkinsRoleRef)}/edit`}>
                      {role.jenkinsRoleRef}
                    </a>
                  </div>
                  {role.jenkinsPermissions.length > 0 && (
                    <div className={styles.roleDetail}>
                      {role.jenkinsPermissions.join(", ")}
                    </div>
                  )}
                </>
              ) : (
                <span className={styles.smallMuted}>No Jenkins role referenced</span>
              )}
            </div>
          </div>
        </Card>
        </div>
      ))}
    </div>
  );
}
