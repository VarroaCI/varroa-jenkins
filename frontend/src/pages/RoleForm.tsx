import { useState, useEffect } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { getRole, createRole, updateRole } from "../api/client";
import type { VarroaRole, APIRule } from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import styles from "./RoleForm.module.css";

export default function RoleForm() {
  const { name } = useParams();
  const navigate = useNavigate();
  const isEdit = !!name;

  const [roleName, setRoleName] = useState("");
  const [apiRules, setApiRules] = useState<APIRule[]>([]);
  const [jenkinsPermissions, setJenkinsPermissions] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetching, setFetching] = useState(isEdit);

  useEffect(() => {
    if (!name) { setFetching(false); return; }
    getRole(name)
      .then((r) => {
        setRoleName(r.metadata.name);
        setApiRules(r.spec.apiRules || []);
        setJenkinsPermissions((r.spec.jenkinsPermissions || []).join("\n"));
      })
      .catch((err) => setError(err.message))
      .finally(() => setFetching(false));
  }, [name]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError("");

    const perms = jenkinsPermissions
      .split("\n")
      .map((p) => p.trim())
      .filter((p) => p.length > 0);

    const role: VarroaRole = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "VarroaRole",
      metadata: { name: roleName },
      spec: { apiRules, jenkinsPermissions: perms },
    };

    try {
      if (isEdit) {
        await updateRole(name!, role);
      } else {
        await createRole(role);
      }
      navigate("/access/roles");
    } catch (err: any) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  const addApiRule = () => setApiRules([...apiRules, { resources: [], verbs: [] }]);
  const updateApiRule = (idx: number, field: "resources" | "verbs", value: string[]) => {
    const updated = [...apiRules];
    updated[idx] = { ...updated[idx], [field]: value };
    setApiRules(updated);
  };
  const removeApiRule = (idx: number) => setApiRules(apiRules.filter((_, i) => i !== idx));

  const resourceOptions = ["*", "controllers", "roles", "rolebindings", "templates", "provisioningdefaults"];
  const verbOptions = ["*", "read", "create", "update", "delete", "manage", "approve-restart", "approve-deletion"];

  if (fetching) return <LoadingSpinner />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>{isEdit ? `Edit Role: ${name}` : "Create Role"}</div>
          <div className={styles.pageDesc}>Define API rules and Jenkins permission grants</div>
        </div>
      </div>

      {error && <div className={styles.errorBanner}>{error}</div>}

      <form onSubmit={handleSubmit}>
        <Card>
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
            <label className={styles.formLabel}>API Rules</label>
            {apiRules.map((rule, idx) => (
              <div key={idx} className={styles.ruleRow}>
                <select
                  multiple
                  className={styles.formSelect}
                  value={rule.resources}
                  onChange={(e) =>
                    updateApiRule(idx, "resources", Array.from(e.target.selectedOptions, (o) => o.value))
                  }
                >
                  {resourceOptions.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
                <select
                  multiple
                  className={styles.formSelect}
                  value={rule.verbs}
                  onChange={(e) =>
                    updateApiRule(idx, "verbs", Array.from(e.target.selectedOptions, (o) => o.value))
                  }
                >
                  {verbOptions.map((v) => (
                    <option key={v} value={v}>
                      {v}
                    </option>
                  ))}
                </select>
                <button type="button" className={styles.removeBtn} onClick={() => removeApiRule(idx)}>
                  &times;
                </button>
              </div>
            ))}
            <button type="button" className={styles.addBtn} onClick={addApiRule}>
              + Add API Rule
            </button>
          </div>

          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Jenkins Permissions (one per line)</label>
            <textarea
              className={styles.formTextarea}
              rows={10}
              value={jenkinsPermissions}
              onChange={(e) => setJenkinsPermissions(e.target.value)}
            />
          </div>

          <div className={styles.actions}>
            <Button variant="primary" type="submit" disabled={loading}>
              {loading ? "Saving..." : "Save"}
            </Button>
            <Button type="button" onClick={() => navigate("/access/roles")}>Cancel</Button>
          </div>
        </Card>
      </form>
    </div>
  );
}
