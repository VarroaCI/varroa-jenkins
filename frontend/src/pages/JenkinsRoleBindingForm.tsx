import { useState, useEffect } from "react";
import { ApiError } from "../hooks/useApi";
import { useParams, useNavigate, useSearchParams } from "react-router-dom";
import {
  getJenkinsRoleBinding,
  createJenkinsRoleBinding,
  updateJenkinsRoleBinding,
  listJenkinsRoles,
} from "../api/client";
import type { JenkinsRoleBinding, JenkinsRole, SubjectRef, JenkinsScope, VarroaRoleBindingScope } from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import ClusterSelector from "../components/ClusterSelector";
import FederatedCopyNote from "../components/FederatedCopyNote";
import { clusterQuery } from "../routing";
import styles from "./JenkinsRoleBindingForm.module.css";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import { FEDERATED_COPY_CODE, federatedCopySource } from "../lib/federatedCopy";

interface LabelEntry {
  key: string;
  value: string;
}

const JENKINS_ROLE_BINDING_CONFLICT_NOTE = "Another user or tool changed this Jenkins role binding after you opened it. Reload the page, then make the change again.";

const MATCH_EXPRESSIONS_NOTE =
  "The dashboard does not edit match expressions. A save keeps them. Use the API or kubectl to change them.";

type MatchExpression = NonNullable<
  NonNullable<VarroaRoleBindingScope["controllerSelector"]>["matchExpressions"]
>[number];

// Formats one stored selector requirement with the operator words of the CRD.
function formatMatchExpression(req: MatchExpression): string {
  const values = req.values && req.values.length > 0 ? ` (${req.values.join(", ")})` : "";
  return `${req.key} ${req.operator}${values}`;
}

export default function JenkinsRoleBindingForm() {
  const { name } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const isEdit = !!name;
  // Edit: the binding lives on a specific cluster (carried in the URL). Create:
  // the cluster is a user choice, seeded from the list page's selection.
  const { cluster, ready } = useConfigurationCluster();

  const [bindingName, setBindingName] = useState("");
  const [subjects, setSubjects] = useState<SubjectRef[]>([{ kind: "Group", name: "" }]);
  const [roleRef, setRoleRef] = useState("");
  const [scopeNamespaces, setScopeNamespaces] = useState("");
  const [selectorLabels, setSelectorLabels] = useState<LabelEntry[]>([]);
  // JenkinsScope fields
  const [jenkinsScopeType, setJenkinsScopeType] = useState<string>("");
  const [jenkinsScopeFolder, setJenkinsScopeFolder] = useState("");
  const [jenkinsScopePropagate, setJenkinsScopePropagate] = useState<string>("");
  const [jenkinsScopePattern, setJenkinsScopePattern] = useState("");
  // Data
  const [roleOptions, setRoleOptions] = useState<JenkinsRole[]>([]);
  const [scopeOpen, setScopeOpen] = useState(false);
  const [jenkinsScopeOpen, setJenkinsScopeOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [fetching, setFetching] = useState(true);
  // The binding as the editor read it. A save sends its metadata, which carries the resourceVersion that fences the write.
  const [loaded, setLoaded] = useState<JenkinsRoleBinding | null>(null);
  // A federated copy is owned by federation. The editor shows it read-only, and the scope sections open.
  const source = loaded ? federatedCopySource("JenkinsRoleBinding", loaded.metadata) : null;
  const readOnly = source !== null;
  const showScope = scopeOpen || readOnly;
  const showJenkinsScope = jenkinsScopeOpen || readOnly;

  useEffect(() => {
    const load = async () => {
      if (!cluster) return;
      try {
        const roles = await listJenkinsRoles(cluster);
        setRoleOptions(roles.items || []);
        if (!name) { setFetching(false); return; }
        const binding = await getJenkinsRoleBinding(cluster, name);
        setLoaded(binding);
        setBindingName(binding.metadata.name);
        setSubjects(binding.spec.subjects || []);
        setRoleRef(binding.spec.roleRef || "");
        setScopeNamespaces(
          (binding.spec.controllerScope?.namespaces || []).join(", ")
        );
        setSelectorLabels(
          Object.entries(
            binding.spec.controllerScope?.controllerSelector?.matchLabels || {}
          ).map(([k, v]) => ({ key: k, value: v }))
        );
        // Jenkins scope
        if (binding.spec.jenkinsScope) {
          setJenkinsScopeType(binding.spec.jenkinsScope.type || "");
          setJenkinsScopeFolder(binding.spec.jenkinsScope.folder || "");
          setJenkinsScopePropagate(binding.spec.jenkinsScope.propagate || "");
          setJenkinsScopePattern(binding.spec.jenkinsScope.pattern || "");
        }
      } catch (err: any) {
        setError(err.message);
      } finally {
        setFetching(false);
      }
    };
    load();
  }, [cluster, name]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (readOnly) return;
    setLoading(true);
    setError("");

    // Build controller scope
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

    // The stored controller scope is the base. Only the fields this form edits
    // are overwritten, so a stored controllerSelector.matchExpressions list
    // survives a save. A dropped selector widens the binding.
    const scope: VarroaRoleBindingScope = { ...loaded?.spec.controllerScope };
    if (namespaces.length > 0) scope.namespaces = namespaces;
    else delete scope.namespaces;
    if (Object.keys(matchLabels).length > 0) {
      scope.controllerSelector = { ...scope.controllerSelector, matchLabels };
    } else if (scope.controllerSelector) {
      const { matchLabels: _removed, ...rest } = scope.controllerSelector;
      if (rest.matchExpressions && rest.matchExpressions.length > 0) scope.controllerSelector = rest;
      else delete scope.controllerSelector;
    }

    // Build Jenkins scope
    let jenkinsScope: JenkinsScope | undefined;
    if (jenkinsScopeType) {
      jenkinsScope = { type: jenkinsScopeType };
      if (jenkinsScopeType === "Folder" && jenkinsScopeFolder)
        jenkinsScope.folder = jenkinsScopeFolder;
      if (jenkinsScopeType === "Pattern" && jenkinsScopePattern)
        jenkinsScope.pattern = jenkinsScopePattern;
      if (jenkinsScopePropagate)
        jenkinsScope.propagate = jenkinsScopePropagate as JenkinsScope["propagate"];
    }

    const binding: JenkinsRoleBinding = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "JenkinsRoleBinding",
      metadata: loaded ? { ...loaded.metadata } : { name: bindingName },
      spec: {
        subjects: subjects.filter((s) => s.name.trim().length > 0),
        roleRef,
        ...(Object.keys(scope).length > 0 ? { controllerScope: scope } : {}),
        ...(jenkinsScope ? { jenkinsScope } : {}),
      },
    };

    try {
      if (isEdit) {
        await updateJenkinsRoleBinding(cluster!, name!, binding);
      } else {
        await createJenkinsRoleBinding(cluster!, binding);
      }
      navigate(`/access/jenkins-role-bindings${clusterQuery(cluster)}`);
    } catch (err: any) {
      if (err instanceof ApiError && err.code === FEDERATED_COPY_CODE) {
        setError(err.serverMessage);
      } else if (err instanceof ApiError && err.status === 409) {
        setError(isEdit ? JENKINS_ROLE_BINDING_CONFLICT_NOTE : "A Jenkins role binding with this name already exists.");
      } else {
        setError(err.message);
      }
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

  // A tenant-member copy names a roleRef that is not a JenkinsRole object. The
  // select keeps that value visible instead of showing an empty choice.
  const roleRefListed = roleOptions.some((r) => r.metadata.name === roleRef);

  const storedExpressions = loaded?.spec.controllerScope?.controllerSelector?.matchExpressions ?? [];

  if (ready && !cluster) return <NoAccessibleClusters />;
  if (fetching || !cluster) return <LoadingSpinner />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>
            {readOnly
              ? `Jenkins Role Binding: ${name}`
              : isEdit ? `Edit Jenkins Role Binding: ${name}` : "Create Jenkins Role Binding"}
          </div>
          <div className={styles.pageDesc}>
            Bind a JenkinsRole to subjects with optional controller and Jenkins scope
          </div>
        </div>
      </div>

      {error && <div className={styles.errorBanner}>{error}</div>}
      {source && <FederatedCopyNote objectNoun="Jenkins role binding" source={source} />}

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
                <button
                  type="button"
                  className={styles.removeBtn}
                  onClick={() => removeSubject(idx)}
                >
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
              <option value="">-- Select a Jenkins role --</option>
              {roleRef && !roleRefListed && <option value={roleRef}>{roleRef}</option>}
              {roleOptions.map((r) => (
                <option key={r.metadata.name} value={r.metadata.name}>
                  {r.metadata.name}
                </option>
              ))}
            </select>
          </div>

          {/* Controller Scope */}
          <div className={styles.formGroup}>
            <button
              type="button"
              className={styles.scopeToggle}
              onClick={() => setScopeOpen(!scopeOpen)}
            >
              {showScope ? "▼" : "▶"} Controller Scope{" "}
              {readOnly ? "" : showScope ? "(click to collapse)" : "(click to expand)"}
            </button>

            {showScope && (
              <div className={styles.scopeBody}>
                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>
                    Namespaces (comma-separated)
                  </label>
                  <input
                    className={styles.formInput}
                    placeholder="namespace-a, namespace-b"
                    value={scopeNamespaces}
                    onChange={(e) => setScopeNamespaces(e.target.value)}
                  />
                  <p className={styles.hint}>
                    {storedExpressions.length > 0
                      ? "Leave empty for cluster-wide scope. The stored selector expressions still narrow this binding."
                      : "Leave empty for cluster-wide scope."}
                  </p>
                </div>

                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>
                    Controller Selector Labels
                  </label>
                  {selectorLabels.map((label, idx) => (
                    <div key={idx} className={styles.labelRow}>
                      <input
                        className={styles.formInput}
                        placeholder="Key"
                        value={label.key}
                        onChange={(e) => updateLabel(idx, "key", e.target.value)}
                      />
                      <span className={styles.labelEquals}>=</span>
                      <input
                        className={styles.formInput}
                        placeholder="Value"
                        value={label.value}
                        onChange={(e) => updateLabel(idx, "value", e.target.value)}
                      />
                      <button
                        type="button"
                        className={styles.removeBtn}
                        onClick={() => removeLabel(idx)}
                      >
                        &times;
                      </button>
                    </div>
                  ))}
                  <button type="button" className={styles.addBtn} onClick={addLabel}>
                    + Add Label
                  </button>
                </div>
                {storedExpressions.length > 0 && (
                  <div className={styles.formGroup}>
                    <div className={styles.formLabel}>Controller Selector Expressions</div>
                    <ul className={styles.exprList}>
                      {storedExpressions.map((req, idx) => (
                        <li key={idx}>{formatMatchExpression(req)}</li>
                      ))}
                    </ul>
                    <p className={styles.hint}>{MATCH_EXPRESSIONS_NOTE}</p>
                  </div>
                )}
              </div>
            )}
          </div>

          {/* Jenkins Scope */}
          <div className={styles.formGroup}>
            <button
              type="button"
              className={styles.scopeToggle}
              onClick={() => setJenkinsScopeOpen(!jenkinsScopeOpen)}
            >
              {showJenkinsScope ? "▼" : "▶"} Jenkins Scope{" "}
              {readOnly ? "" : showJenkinsScope ? "(click to collapse)" : "(click to expand)"}
            </button>

            {showJenkinsScope && (
              <div className={styles.scopeBody}>
                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>Type</label>
                  <select
                    className={styles.formSelect}
                    value={jenkinsScopeType}
                    onChange={(e) => setJenkinsScopeType(e.target.value)}
                  >
                    <option value="">-- No Jenkins scope --</option>
                    <option value="Global">Global</option>
                    <option value="Folder">Folder</option>
                    <option value="Pattern">Pattern</option>
                  </select>
                </div>

                {jenkinsScopeType === "Folder" && (
                  <div className={styles.formGroup}>
                    <label className={styles.formLabel}>Folder</label>
                    <input
                      className={styles.formInput}
                      placeholder="e.g. /tenants/backend"
                      value={jenkinsScopeFolder}
                      onChange={(e) => setJenkinsScopeFolder(e.target.value)}
                    />
                  </div>
                )}

                {jenkinsScopeType === "Pattern" && (
                  <div className={styles.formGroup}>
                    <label className={styles.formLabel}>Pattern</label>
                    <input
                      className={styles.formInput}
                      placeholder="e.g. .*tenant-a.*"
                      value={jenkinsScopePattern}
                      onChange={(e) => setJenkinsScopePattern(e.target.value)}
                    />
                  </div>
                )}

                {jenkinsScopeType && (
                  <div className={styles.formGroup}>
                    <label className={styles.formLabel}>Propagate</label>
                    <select
                      className={styles.formSelect}
                      value={jenkinsScopePropagate}
                      onChange={(e) => setJenkinsScopePropagate(e.target.value)}
                    >
                      <option value="">None</option>
                      <option value="Children">Children</option>
                      <option value="Subtree">Subtree</option>
                    </select>
                  </div>
                )}
              </div>
            )}
          </div>
          </fieldset>

          <div className={styles.actions}>
            {!readOnly && (
              <Button variant="primary" type="submit" disabled={loading}>
                {loading ? "Saving..." : "Save"}
              </Button>
            )}
            <Button type="button" onClick={() => navigate(`/access/jenkins-role-bindings${clusterQuery(cluster)}`)}>
              {readOnly ? "Back" : "Cancel"}
            </Button>
          </div>
        </Card>
      </form>
    </div>
  );
}
