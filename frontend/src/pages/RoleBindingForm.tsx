import { useState, useEffect } from "react";
import { ApiError } from "../hooks/useApi";
import { useParams, useNavigate } from "react-router-dom";
import { getRoleBinding, createRoleBinding, updateRoleBinding, listRoles } from "../api/client";
import type { VarroaRoleBinding, VarroaRole, SubjectRef, VarroaRoleBindingScope, VarroaRoleBindingStatus } from "../types";
import type { TenantScope } from "../types/auth";
import LoadingSpinner from "../components/LoadingSpinner";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import TenantScopeEditor, { validateTenantScope } from "../components/TenantScopeEditor";
import { FederationStatusTable } from "../components/FederationStatus";
import styles from "./RoleBindingForm.module.css";

interface LabelEntry {
  key: string;
  value: string;
}

const TENANTS_DISABLED_REASON =
  "A scope names one dimension. Clear the namespaces and the selector labels to scope by tenant.";
const OTHERS_DISABLED_REASON =
  "A scope names one dimension. Remove every tenant entry to scope by namespace or by selector.";

export default function RoleBindingForm() {
  const { name } = useParams();
  const navigate = useNavigate();
  const isEdit = !!name;

  const [bindingName, setBindingName] = useState("");
  const [subjects, setSubjects] = useState<SubjectRef[]>([{ kind: "Group", name: "" }]);
  const [roleRef, setRoleRef] = useState("");
  const [scopeNamespaces, setScopeNamespaces] = useState("");
  const [selectorLabels, setSelectorLabels] = useState<LabelEntry[]>([]);
  // The whole loaded scope is held, not only the dimensions this form
  // edits. A submit spreads this value, so a stored dimension the form does
  // not edit survives the edit.
  const [loadedScope, setLoadedScope] = useState<VarroaRoleBindingScope | undefined>(undefined);
  const [tenants, setTenants] = useState<TenantScope[] | undefined>(undefined);
  // Read-only. The core BFF writes the status, and a submit never sends it.
  const [federation, setFederation] = useState<VarroaRoleBindingStatus | undefined>(undefined);
  const [roleOptions, setRoleOptions] = useState<VarroaRole[]>([]);
  const [scopeOpen, setScopeOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetching, setFetching] = useState(true);

  useEffect(() => {
    const load = async () => {
      try {
        const roles = await listRoles();
        setRoleOptions(roles.items || []);
        if (!name) { setFetching(false); return; }
        const binding = await getRoleBinding(name);
        setBindingName(binding.metadata.name);
        setSubjects(binding.spec.subjects || []);
        setRoleRef(binding.spec.roleRef || "");
        setLoadedScope(binding.spec.scope);
        setTenants(binding.spec.scope?.tenants);
        setFederation(binding.status);
        setScopeNamespaces((binding.spec.scope?.namespaces || []).join(", "));
        setSelectorLabels(
          Object.entries(binding.spec.scope?.controllerSelector?.matchLabels || {}).map(([k, v]) => ({
            key: k,
            value: v,
          }))
        );
      } catch (err: any) {
        setError(err.message);
      } finally {
        setFetching(false);
      }
    };
    load();
  }, [name]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    const tenantErrors = validateTenantScope(tenants);
    if (tenantErrors.length > 0) {
      setError(tenantErrors[0]);
      return;
    }

    setLoading(true);
    setError("");

    const namespaces = scopeNamespaces
      .split(",")
      .map((n) => n.trim())
      .filter((n) => n.length > 0);

    const matchLabels: Record<string, string> = {};
    selectorLabels
      .filter((l) => l.key.trim().length > 0)
      .forEach((l) => {
        matchLabels[l.key.trim()] = l.value.trim();
      });

    // The stored scope is the base. Only the dimensions this form edits are
    // overwritten, so a stored controllerSelector.matchExpressions list and
    // any other dimension survive an edit that does not touch it.
    const scope: VarroaRoleBindingScope = { ...loadedScope };
    if (namespaces.length > 0) scope.namespaces = namespaces;
    else delete scope.namespaces;
    if (Object.keys(matchLabels).length > 0) {
      scope.controllerSelector = { ...scope.controllerSelector, matchLabels };
    } else if (scope.controllerSelector) {
      const { matchLabels: _removed, ...rest } = scope.controllerSelector;
      if (rest.matchExpressions && rest.matchExpressions.length > 0) scope.controllerSelector = rest;
      else delete scope.controllerSelector;
    }
    if (tenants && tenants.length > 0) scope.tenants = tenants;
    else delete scope.tenants;

    const binding: VarroaRoleBinding = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "VarroaRoleBinding",
      metadata: { name: bindingName },
      spec: {
        subjects: subjects.filter((s) => s.name.trim().length > 0),
        roleRef,
        scope: Object.keys(scope).length > 0 ? scope : undefined,
      },
    };

    try {
      if (isEdit) {
        await updateRoleBinding(name!, binding);
      } else {
        await createRoleBinding(binding);
      }
      navigate("/access/role-bindings");
    } catch (err: any) {
      setError(!isEdit && err instanceof ApiError && err.status === 409 ? "A role binding with this name already exists." : err.message);
    } finally {
      setLoading(false);
    }
  };

  const updateSubject = (idx: number, field: "kind" | "name", value: string) => {
    setSubjects((prev) => {
      const next = [...prev];
      next[idx] = { ...next[idx], [field]: value };
      return next;
    });
  };

  const addSubject = () => setSubjects([...subjects, { kind: "Group", name: "" }]);
  const removeSubject = (idx: number) =>
    setSubjects(subjects.length > 1 ? subjects.filter((_, i) => i !== idx) : subjects);

  const updateLabel = (idx: number, field: "key" | "value", value: string) => {
    setSelectorLabels((prev) => {
      const next = [...prev];
      next[idx] = { ...next[idx], [field]: value };
      return next;
    });
  };
  const addLabel = () => setSelectorLabels([...selectorLabels, { key: "", value: "" }]);
  const removeLabel = (idx: number) =>
    setSelectorLabels(selectorLabels.filter((_, i) => i !== idx));

  // The one-dimension rule: a scope names tenants, or namespaces and a
  // selector, never both. The API server's own rule stays the authority.
  const hasTenants = (tenants?.length ?? 0) > 0;
  const hasOtherDimension =
    scopeNamespaces.trim().length > 0 || selectorLabels.some((l) => l.key.trim().length > 0);

  if (fetching) return <LoadingSpinner />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>{isEdit ? `Edit Role Binding: ${name}` : "Create Role Binding"}</div>
          <div className={styles.pageDesc}>Bind roles to users and groups with optional scope constraints</div>
        </div>
      </div>

      {error && <div className={styles.errorBanner}>{error}</div>}

      <form onSubmit={handleSubmit}>
        <Card>
          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Name</label>
            <input
              className={styles.formInput}
              value={bindingName}
              onChange={(e) => setBindingName(e.target.value)}
              disabled={isEdit}
              required
            />
          </div>

          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Subjects</label>
            {subjects.map((subj, idx) => (
              <div key={idx} className={styles.subjectRow}>
                <select
                  className={styles.formSelect}
                  value={subj.kind}
                  onChange={(e) => updateSubject(idx, "kind", e.target.value)}
                >
                  <option value="Group">Group</option>
                  <option value="User">User</option>
                </select>
                <input
                  className={styles.formInput}
                  placeholder="Subject name"
                  value={subj.name}
                  onChange={(e) => updateSubject(idx, "name", e.target.value)}
                />
                <button type="button" className={styles.removeBtn} onClick={() => removeSubject(idx)}>
                  &times;
                </button>
              </div>
            ))}
            <button type="button" className={styles.addBtn} onClick={addSubject}>
              + Add Subject
            </button>
          </div>

          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Role Ref</label>
            <select
              className={styles.formSelect}
              value={roleRef}
              onChange={(e) => setRoleRef(e.target.value)}
              required
            >
              <option value="">-- Select a role --</option>
              {roleOptions.map((r) => (
                <option key={r.metadata.name} value={r.metadata.name}>
                  {r.metadata.name}
                </option>
              ))}
            </select>
          </div>

          <div className={styles.formGroup}>
            <button
              type="button"
              className={styles.scopeToggle}
              onClick={() => setScopeOpen(!scopeOpen)}
            >
              {scopeOpen ? "▼" : "▶"} Scope {scopeOpen ? "(click to collapse)" : "(click to expand)"}
            </button>

            {scopeOpen && (
              <div className={styles.scopeBody}>
                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>Namespaces (comma-separated)</label>
                  <input
                    className={styles.formInput}
                    placeholder="namespace-a, namespace-b"
                    value={scopeNamespaces}
                    disabled={hasTenants}
                    onChange={(e) => setScopeNamespaces(e.target.value)}
                  />
                  <p className={styles.hint}>
                    {hasTenants ? OTHERS_DISABLED_REASON : "Leave empty for cluster-wide scope."}
                  </p>
                </div>

                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>Controller Selector Labels</label>
                  {selectorLabels.map((label, idx) => (
                    <div key={idx} className={styles.labelRow}>
                      <input
                        className={styles.formInput}
                        placeholder="Key"
                        value={label.key}
                        disabled={hasTenants}
                        onChange={(e) => updateLabel(idx, "key", e.target.value)}
                      />
                      <span className={styles.labelEquals}>=</span>
                      <input
                        className={styles.formInput}
                        placeholder="Value"
                        value={label.value}
                        disabled={hasTenants}
                        onChange={(e) => updateLabel(idx, "value", e.target.value)}
                      />
                      <button type="button" className={styles.removeBtn} onClick={() => removeLabel(idx)}>
                        &times;
                      </button>
                    </div>
                  ))}
                  <button
                    type="button"
                    className={styles.addBtn}
                    disabled={hasTenants}
                    onClick={addLabel}
                  >
                    + Add Label
                  </button>
                  {hasTenants && <p className={styles.hint}>{OTHERS_DISABLED_REASON}</p>}
                </div>

                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>Pipelines Tenants</label>
                  <TenantScopeEditor
                    value={tenants}
                    onChange={setTenants}
                    disabled={hasOtherDimension}
                    disabledReason={TENANTS_DISABLED_REASON}
                  />
                </div>
              </div>
            )}
          </div>

          <div className={styles.actions}>
            <Button variant="primary" type="submit" disabled={loading}>
              {loading ? "Saving..." : "Save"}
            </Button>
            <Button type="button" onClick={() => navigate("/access/role-bindings")}>Cancel</Button>
          </div>
        </Card>
      </form>

      {isEdit && (
        <Card>
          <FederationStatusTable status={federation} />
        </Card>
      )}
    </div>
  );
}
