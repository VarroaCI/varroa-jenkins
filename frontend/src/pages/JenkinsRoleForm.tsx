import { useState, useEffect } from "react";
import { ApiError } from "../hooks/useApi";
import { useParams, useNavigate, useSearchParams } from "react-router-dom";
import { getJenkinsRole, createJenkinsRole, updateJenkinsRole } from "../api/client";
import type { JenkinsRole, JenkinsRoleType } from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import ClusterSelector from "../components/ClusterSelector";
import FederatedCopyNote from "../components/FederatedCopyNote";
import { clusterQuery } from "../routing";
import styles from "./JenkinsRoleForm.module.css";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import { FEDERATED_COPY_CODE, federatedCopySource } from "../lib/federatedCopy";

const ROLE_TYPES: JenkinsRoleType[] = ["Global", "Item", "Agent"];

const JENKINS_ROLE_CONFLICT_NOTE = "Another user or tool changed this Jenkins role after you opened it. Reload the page, then make the change again.";

export default function JenkinsRoleForm() {
  const { name } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const isEdit = !!name;
  // Edit: the role already lives on a specific cluster (carried in the URL).
  // Create: the cluster is a user choice, seeded from the list page's selection.
  const { cluster, ready } = useConfigurationCluster();

  const [roleName, setRoleName] = useState("");
  const [roleType, setRoleType] = useState<JenkinsRoleType>("Global");
  const [permissions, setPermissions] = useState("");
  const [description, setDescription] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetching, setFetching] = useState(isEdit);
  // The role as the editor read it. A save sends its metadata, which carries the resourceVersion that fences the write.
  const [loaded, setLoaded] = useState<JenkinsRole | null>(null);
  // A federated copy is owned by federation. The editor shows it read-only.
  const source = loaded ? federatedCopySource("JenkinsRole", loaded.metadata) : null;
  const readOnly = source !== null;

  useEffect(() => {
    if (!cluster) return;
    if (!name) { setFetching(false); return; }
    getJenkinsRole(cluster, name)
      .then((r) => {
        setLoaded(r);
        setRoleName(r.metadata.name);
        setRoleType(r.spec.roleType || "Global");
        setPermissions((r.spec.permissions || []).join("\n"));
        setDescription(r.spec.description || "");
      })
      .catch((err) => setError(err.message))
      .finally(() => setFetching(false));
  }, [cluster, name]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (readOnly) return;
    setLoading(true);
    setError("");

    const perms = permissions
      .split("\n")
      .map((p) => p.trim())
      .filter((p) => p.length > 0);

    const role: JenkinsRole = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "JenkinsRole",
      metadata: loaded ? { ...loaded.metadata } : { name: roleName },
      spec: { roleType, permissions: perms, ...(description ? { description } : {}) },
    };

    try {
      if (isEdit) {
        await updateJenkinsRole(cluster!, name!, role);
      } else {
        await createJenkinsRole(cluster!, role);
      }
      navigate(`/access/jenkins-roles${clusterQuery(cluster)}`);
    } catch (err: any) {
      if (err instanceof ApiError && err.code === FEDERATED_COPY_CODE) {
        setError(err.serverMessage);
      } else if (err instanceof ApiError && err.status === 409) {
        setError(isEdit ? JENKINS_ROLE_CONFLICT_NOTE : "A Jenkins role with this name already exists.");
      } else {
        setError(err.message);
      }
    } finally {
      setLoading(false);
    }
  };

  if (ready && !cluster) return <NoAccessibleClusters />;
  if (fetching || !cluster) return <LoadingSpinner />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>
            {readOnly ? `Jenkins Role: ${name}` : isEdit ? `Edit Jenkins Role: ${name}` : "Create Jenkins Role"}
          </div>
          <div className={styles.pageDesc}>Define a pure Jenkins permission set (Global, Item, or Agent)</div>
        </div>
      </div>

      {error && <div className={styles.errorBanner}>{error}</div>}
      {source && <FederatedCopyNote objectNoun="Jenkins role" source={source} />}

      <form onSubmit={handleSubmit}>
        <Card>
          <fieldset disabled={readOnly} className={styles.fieldset}>
            {!isEdit && (
              <div className={styles.formGroup}>
                <ClusterSelector value={cluster} onChange={(value) => {
                  const next = new URLSearchParams(searchParams);
                  next.set("cluster", value);
                  setSearchParams(next);
                }} />
              </div>
            )}

            <div className={styles.formGroup}>
              <label className={styles.formLabel}>Name</label>
              <input
                className={styles.formInput}
                value={roleName}
                onChange={(e) => setRoleName(e.target.value)}
                disabled={isEdit}
                required
              />
            </div>

            <div className={styles.formGroup}>
              <label className={styles.formLabel}>Role Type</label>
              <select
                className={styles.formSelect}
                value={roleType}
                onChange={(e) => setRoleType(e.target.value as JenkinsRoleType)}
              >
                {ROLE_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </div>

            <div className={styles.formGroup}>
              <label className={styles.formLabel}>Permissions (one per line)</label>
              <textarea
                className={styles.formTextarea}
                rows={10}
                placeholder="hudson.model.Hudson.Read&#10;hudson.model.Item.Build&#10;hudson.model.Computer.Configure"
                value={permissions}
                onChange={(e) => setPermissions(e.target.value)}
              />
            </div>

            <div className={styles.formGroup}>
              <label className={styles.formLabel}>Description</label>
              <input
                className={styles.formInput}
                placeholder="Optional description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
            </div>
          </fieldset>

          <div className={styles.actions}>
            {!readOnly && (
              <Button variant="primary" type="submit" disabled={loading}>
                {loading ? "Saving..." : "Save"}
              </Button>
            )}
            <Button type="button" onClick={() => navigate(`/access/jenkins-roles${clusterQuery(cluster)}`)}>
              {readOnly ? "Back" : "Cancel"}
            </Button>
          </div>
        </Card>
      </form>
    </div>
  );
}
