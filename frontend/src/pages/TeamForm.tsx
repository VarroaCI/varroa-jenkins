import { useState, useEffect } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { getTeam, createTeam, updateTeam, listRoles, listComposedBundles } from "../api/client";
import type { TeamEntry } from "../api/client";
import type { ComposedBundle } from "../types";
import { teamMemberLabel } from "../types";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { useClusters } from "../hooks/useClusters";
import styles from "./TeamForm.module.css";

interface SubjectRef {
  kind: "Group" | "User";
  name: string;
}

/**
 * ExportFormEntry is the form's own shape for one spec.pipelines.exports
 * entry. kind picks which of job/locks the entry submits; the other is
 * dropped on save. locks and callers always keep at least one editable
 * row, matching the members/namespaces array editors above.
 */
interface ExportFormEntry {
  kind: "job" | "locks";
  job: string;
  locks: string[];
  callers: string[];
}

function emptyExport(): ExportFormEntry {
  return { kind: "job", job: "", locks: [""], callers: [""] };
}

interface FormData {
  name: string;
  displayName: string;
  members: string[];
  subjects: SubjectRef[];
  namespaces: string[];
  roleRef: string;
  provisionNamespaces: boolean;
  pipelinesEnabled: boolean;
  pipelinesProfile: string;
  pipelinesCellPool: string;
  pipelinesAgentNamespace: string;
  pipelinesBundleName: string;
  pipelinesBundleNamespace: string;
  pipelinesRehydrateBuilds: string;
  pipelinesRehydrateDeadline: string;
  pipelinesInterruptedRetry: "manual" | "automatic";
  pipelinesConcurrency: "allow" | "cancelInProgress";
  pipelinesCronBackstop: boolean;
  pipelinesCronCatchUp: "one" | "none";
  pipelinesCronGrace: string;
  pipelinesExports: ExportFormEntry[];
}

function emptyForm(): FormData {
  return {
    name: "",
    displayName: "",
    members: [""],
    subjects: [{ kind: "Group" as const, name: "" }],
    namespaces: [""],
    roleRef: "developer",
    provisionNamespaces: false,
    pipelinesEnabled: false,
    pipelinesProfile: "",
    pipelinesCellPool: "",
    pipelinesAgentNamespace: "",
    pipelinesBundleName: "",
    pipelinesBundleNamespace: "",
    pipelinesRehydrateBuilds: "",
    pipelinesRehydrateDeadline: "",
    pipelinesInterruptedRetry: "manual",
    pipelinesConcurrency: "allow",
    pipelinesCronBackstop: true,
    pipelinesCronCatchUp: "one",
    pipelinesCronGrace: "",
    pipelinesExports: [],
  };
}

function formFromTeam(team: TeamEntry): FormData {
  return {
    name: team.name,
    displayName: team.displayName || "",
    members: team.members && team.members.length > 0 ? team.members : [""],
    subjects: team.subjects && team.subjects.length > 0 ? team.subjects : [{ kind: "Group", name: "" }],
    namespaces: team.namespaces.length > 0 ? team.namespaces : [""],
    roleRef: team.roleRef || "developer",
    provisionNamespaces: team.provisionNamespaces || false,
    pipelinesEnabled: !!team.pipelines,
    pipelinesProfile: team.pipelines?.profile || "",
    pipelinesCellPool: team.pipelines?.cellPool || "",
    pipelinesAgentNamespace: team.pipelines?.agentNamespace || "",
    pipelinesBundleName: team.pipelines?.composedBundleRef?.name || "",
    pipelinesBundleNamespace: team.pipelines?.composedBundleRef?.namespace || "",
    pipelinesRehydrateBuilds:
      team.pipelines?.rehydrateBuilds !== undefined ? String(team.pipelines.rehydrateBuilds) : "",
    pipelinesRehydrateDeadline: team.pipelines?.rehydrateDeadline || "",
    pipelinesInterruptedRetry: team.pipelines?.interruptedRetry || "manual",
    pipelinesConcurrency: team.pipelines?.concurrency || "allow",
    pipelinesCronBackstop: team.pipelines?.cronBackstop !== false,
    pipelinesCronCatchUp: team.pipelines?.cronCatchUp || "one",
    pipelinesCronGrace: team.pipelines?.cronGrace || "",
    pipelinesExports: (team.pipelines?.exports ?? []).map((exp) => ({
      kind: exp.locks && exp.locks.length > 0 ? ("locks" as const) : ("job" as const),
      job: exp.job || "",
      locks: exp.locks && exp.locks.length > 0 ? exp.locks : [""],
      callers: exp.callers && exp.callers.length > 0 ? exp.callers : [""],
    })),
  };
}

/** Reads one condition of the given type off a Team, or undefined if absent. */
function condition(team: TeamEntry | null, type: string) {
  return team?.conditions?.find((c) => c.type === type);
}

export default function TeamForm() {
  const navigate = useNavigate();
  const { name } = useParams<{ name: string }>();
  const isEdit = !!name;

  const [form, setForm] = useState<FormData>(emptyForm);
  const [team, setTeam] = useState<TeamEntry | null>(null);
  const [availableRoles, setAvailableRoles] = useState<string[]>(["developer", "viewer", "editor"]);
  const [loading, setLoading] = useState(isEdit);
  const [saving, setSaving] = useState(false);
  const [errors, setErrors] = useState<string[]>([]);

  const { data: clusters } = useClusters();
  const [bundleCluster, setBundleCluster] = useState("");
  const [bundles, setBundles] = useState<ComposedBundle[]>([]);

  useEffect(() => {
    listRoles()
      .then((data: { items: Array<{ metadata: { name: string } }> }) => {
        const roles = (data.items || [])
          .map((r) => r.metadata.name)
          .filter((n) => n !== "admin");
        if (roles.length > 0) setAvailableRoles(roles);
      })
      .catch(() => {
        // fallback to defaults
      });
  }, []);

  useEffect(() => {
    if (isEdit) {
      getTeam(name!)
        .then((t) => {
          setTeam(t);
          setForm(formFromTeam(t));
          setLoading(false);
        })
        .catch(() => {
          setErrors(["Team not found"]);
          setLoading(false);
        });
    }
  }, [isEdit, name]);

  // The bundle picker lists a cluster's ComposedBundles so the admin can
  // choose one for spec.pipelines.composedBundleRef; the namespace comes
  // from the chosen bundle's own metadata, never guessed.
  useEffect(() => {
    if (!bundleCluster) {
      setBundles([]);
      return;
    }
    listComposedBundles(bundleCluster)
      .then((data) => setBundles(data.items ?? []))
      .catch(() => setBundles([]));
  }, [bundleCluster]);

  const updateField = <K extends keyof FormData>(key: K, value: FormData[K]) => {
    setForm((prev) => ({ ...prev, [key]: value }));
  };

  const updateArrayItem = (field: "members" | "namespaces", index: number, value: string) => {
    setForm((prev) => {
      const arr = [...prev[field]];
      arr[index] = value;
      return { ...prev, [field]: arr };
    });
  };

  const addArrayItem = (field: "members" | "namespaces") => {
    setForm((prev) => ({ ...prev, [field]: [...prev[field], ""] }));
  };

  const removeArrayItem = (field: "members" | "namespaces", index: number) => {
    setForm((prev) => {
      const arr = prev[field].filter((_, i) => i !== index);
      return { ...prev, [field]: arr.length === 0 ? [""] : arr };
    });
  };

  const updateSubject = (index: number, field: "kind" | "name", value: string) => {
    setForm((prev) => {
      const subjects = [...prev.subjects];
      subjects[index] = { ...subjects[index], [field]: value };
      return { ...prev, subjects };
    });
  };

  const addSubject = () => {
    setForm((prev) => ({
      ...prev,
      subjects: [...prev.subjects, { kind: "Group" as const, name: "" }],
    }));
  };

  const removeSubject = (index: number) => {
    setForm((prev) => {
      const subjects = prev.subjects.filter((_, i) => i !== index);
      return { ...prev, subjects: subjects.length === 0 ? [{ kind: "Group", name: "" }] : subjects };
    });
  };

  const addExport = () => {
    setForm((prev) => ({ ...prev, pipelinesExports: [...prev.pipelinesExports, emptyExport()] }));
  };

  const removeExport = (index: number) => {
    setForm((prev) => ({
      ...prev,
      pipelinesExports: prev.pipelinesExports.filter((_, i) => i !== index),
    }));
  };

  const updateExportKind = (index: number, kind: "job" | "locks") => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      exports[index] = { ...exports[index], kind };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const updateExportJob = (index: number, value: string) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      exports[index] = { ...exports[index], job: value };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const updateExportLock = (index: number, lockIndex: number, value: string) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      const locks = [...exports[index].locks];
      locks[lockIndex] = value;
      exports[index] = { ...exports[index], locks };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const addExportLock = (index: number) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      exports[index] = { ...exports[index], locks: [...exports[index].locks, ""] };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const removeExportLock = (index: number, lockIndex: number) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      const locks = exports[index].locks.filter((_, i) => i !== lockIndex);
      exports[index] = { ...exports[index], locks: locks.length === 0 ? [""] : locks };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const updateExportCaller = (index: number, callerIndex: number, value: string) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      const callers = [...exports[index].callers];
      callers[callerIndex] = value;
      exports[index] = { ...exports[index], callers };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const addExportCaller = (index: number) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      exports[index] = { ...exports[index], callers: [...exports[index].callers, ""] };
      return { ...prev, pipelinesExports: exports };
    });
  };

  const removeExportCaller = (index: number, callerIndex: number) => {
    setForm((prev) => {
      const exports = [...prev.pipelinesExports];
      const callers = exports[index].callers.filter((_, i) => i !== callerIndex);
      exports[index] = { ...exports[index], callers: callers.length === 0 ? [""] : callers };
      return { ...prev, pipelinesExports: exports };
    });
  };

  // Selecting a bundle records its namespace explicitly: the <option> value
  // packs namespace and name together so two bundles that share a name
  // across namespaces never collide, and the split writes both form fields.
  const bundleOptionValue = (b: ComposedBundle) => `${b.metadata.namespace || ""}::${b.metadata.name}`;

  const onBundleChange = (value: string) => {
    if (!value) {
      updateField("pipelinesBundleName", "");
      updateField("pipelinesBundleNamespace", "");
      return;
    }
    const [ns, ...rest] = value.split("::");
    setForm((prev) => ({ ...prev, pipelinesBundleName: rest.join("::"), pipelinesBundleNamespace: ns }));
  };

  const validate = (): string[] => {
    const errs: string[] = [];
    if (!isEdit && !form.name.trim()) errs.push("Name is required");
    const hasMembers = form.members.some((m) => m.trim());
    const hasSubjects = form.subjects.some((s) => s.name.trim());
    if (!hasMembers && !hasSubjects) errs.push("At least one member or subject is required");
    const hasNs = form.namespaces.some((n) => n.trim());
    if (!hasNs) errs.push("At least one namespace is required");
    if (form.roleRef === "admin") errs.push("Role 'admin' is not permitted");
    if (form.pipelinesEnabled && form.pipelinesAgentNamespace) {
      const managed = form.namespaces.map((n) => n.trim()).filter(Boolean);
      if (!managed.includes(form.pipelinesAgentNamespace)) {
        errs.push("Agent namespace must be one of this team's namespaces");
      }
    }
    if (form.pipelinesEnabled && form.pipelinesRehydrateBuilds.trim()) {
      const n = Number(form.pipelinesRehydrateBuilds);
      if (!Number.isInteger(n) || n < 0 || n > 100) {
        errs.push("Rehydrate builds must be a whole number between 0 and 100");
      }
    }
    if (form.pipelinesEnabled) {
      form.pipelinesExports.forEach((exp, i) => {
        if (!exp.callers.some((c) => c.trim())) {
          errs.push(`Export ${i + 1}: at least one caller is required`);
        }
        if (exp.kind === "job") {
          if (!exp.job.trim()) errs.push(`Export ${i + 1}: a job pattern is required`);
          else if (exp.job.trim().startsWith("/")) errs.push(`Export ${i + 1}: job pattern must not start with /`);
        } else if (!exp.locks.some((l) => l.trim())) {
          errs.push(`Export ${i + 1}: at least one lock pattern is required`);
        }
      });
    }
    return errs;
  };

  const handleSubmit = async () => {
    const errs = validate();
    setErrors(errs);
    if (errs.length > 0) return;

    setSaving(true);
    try {
      const body = {
        name: form.name,
        displayName: form.displayName || undefined,
        members: form.members.filter((m) => m.trim()),
        subjects: form.subjects.filter((s) => s.name.trim()),
        namespaces: form.namespaces.filter((n) => n.trim()),
        roleRef: form.roleRef || undefined,
        provisionNamespaces: form.provisionNamespaces,
        pipelines: form.pipelinesEnabled
          ? {
              profile: form.pipelinesProfile.trim() || undefined,
              cellPool: form.pipelinesCellPool.trim() || undefined,
              agentNamespace: form.pipelinesAgentNamespace.trim() || undefined,
              composedBundleRef: form.pipelinesBundleName
                ? { name: form.pipelinesBundleName, namespace: form.pipelinesBundleNamespace }
                : undefined,
              rehydrateBuilds: form.pipelinesRehydrateBuilds.trim()
                ? Number(form.pipelinesRehydrateBuilds)
                : undefined,
              rehydrateDeadline: form.pipelinesRehydrateDeadline.trim() || undefined,
              interruptedRetry: form.pipelinesInterruptedRetry,
              concurrency: form.pipelinesConcurrency,
              cronBackstop: form.pipelinesCronBackstop,
              cronCatchUp: form.pipelinesCronCatchUp,
              cronGrace: form.pipelinesCronGrace.trim() || undefined,
              exports:
                form.pipelinesExports.length > 0
                  ? form.pipelinesExports.map((exp) =>
                      exp.kind === "job"
                        ? { job: exp.job.trim(), callers: exp.callers.map((c) => c.trim()).filter(Boolean) }
                        : {
                            locks: exp.locks.map((l) => l.trim()).filter(Boolean),
                            callers: exp.callers.map((c) => c.trim()).filter(Boolean),
                          },
                    )
                  : undefined,
            }
          : undefined,
      };

      if (isEdit) {
        await updateTeam(name!, body);
      } else {
        await createTeam(body);
      }
      navigate("/access/teams");
    } catch (err: unknown) {
      setErrors([err instanceof Error ? err.message : "Save failed"]);
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <p>Loading...</p>;

  const placedCond = condition(team, "Placed");
  const warningsCond = condition(team, "TenantWarnings");
  const rehydratedCond = condition(team, "Rehydrated");

  return (
    <div className={styles.teamsPage}>
      <h1>{isEdit ? "Edit Team" : "Create Team"}</h1>

      {errors.length > 0 && (
        <div className={styles.errors}>
          {errors.map((e, i) => (
            <p key={i}>{e}</p>
          ))}
        </div>
      )}

      <div className={styles.form}>
        {!isEdit && (
          <div className={styles.field}>
            <label>Name</label>
            <input
              type="text"
              value={form.name}
              onChange={(e) => updateField("name", e.target.value)}
            />
          </div>
        )}

        <div className={styles.field}>
          <label>Display Name</label>
          <input
            type="text"
            value={form.displayName}
            onChange={(e) => updateField("displayName", e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <label>Members (local usernames)</label>
          <div className={styles.arrayEditor}>
            {form.members.map((m, i) => (
              <div key={i} className={styles.arrayRow}>
                <input
                  type="text"
                  value={m}
                  onChange={(e) => updateArrayItem("members", i, e.target.value)}
                  placeholder="Username"
                />
                <button
                  className={styles.removeBtn}
                  onClick={() => removeArrayItem("members", i)}
                >
                  ×
                </button>
              </div>
            ))}
            <button className={styles.addBtn} onClick={() => addArrayItem("members")}>
              + Add member
            </button>
          </div>
        </div>

        <div className={styles.field}>
          <label>Subjects (IdP groups/users)</label>
          <div className={styles.arrayEditor}>
            {form.subjects.map((s, i) => (
              <div key={i} className={styles.subjectRow}>
                <select
                  value={s.kind}
                  onChange={(e) => updateSubject(i, "kind", e.target.value)}
                >
                  <option value="Group">Group</option>
                  <option value="User">User</option>
                </select>
                <input
                  type="text"
                  value={s.name}
                  onChange={(e) => updateSubject(i, "name", e.target.value)}
                  placeholder="Name"
                />
                <button
                  className={styles.removeBtn}
                  onClick={() => removeSubject(i)}
                >
                  ×
                </button>
              </div>
            ))}
            <button className={styles.addBtn} onClick={addSubject}>
              + Add subject
            </button>
          </div>
        </div>

        <div className={styles.field}>
          <label>Namespaces (at least one required)</label>
          <div className={styles.arrayEditor}>
            {form.namespaces.map((ns, i) => (
              <div key={i} className={styles.arrayRow}>
                <input
                  type="text"
                  value={ns}
                  onChange={(e) => updateArrayItem("namespaces", i, e.target.value)}
                  placeholder="Namespace"
                />
                <button
                  className={styles.removeBtn}
                  onClick={() => removeArrayItem("namespaces", i)}
                >
                  ×
                </button>
              </div>
            ))}
            <button className={styles.addBtn} onClick={() => addArrayItem("namespaces")}>
              + Add namespace
            </button>
          </div>
        </div>

        <div className={styles.field}>
          <label>Role</label>
          <select
            value={form.roleRef}
            onChange={(e) => updateField("roleRef", e.target.value)}
          >
            {availableRoles.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </div>

        <div className={styles.toggleRow}>
          <input
            type="checkbox"
            id="provisionNamespaces"
            checked={form.provisionNamespaces}
            onChange={(e) => updateField("provisionNamespaces", e.target.checked)}
          />
          <label htmlFor="provisionNamespaces">Provision namespaces (create if missing)</label>
        </div>

        <div className={styles.toggleRow}>
          <input
            type="checkbox"
            id="pipelinesEnabled"
            checked={form.pipelinesEnabled}
            onChange={(e) => updateField("pipelinesEnabled", e.target.checked)}
          />
          <label htmlFor="pipelinesEnabled">Pipelines tenant (place this team on a cell)</label>
        </div>

        {form.pipelinesEnabled && (
          <div className={styles.pipelinesSection}>
            <div className={styles.field}>
              <label>Version profile</label>
              <input
                type="text"
                value={form.pipelinesProfile}
                onChange={(e) => updateField("pipelinesProfile", e.target.value)}
                placeholder="JenkinsVersionProfile name"
              />
            </div>

            <div className={styles.field}>
              <label>Cell pool</label>
              <input
                type="text"
                value={form.pipelinesCellPool}
                onChange={(e) => updateField("pipelinesCellPool", e.target.value)}
                placeholder="CellPool name (optional)"
              />
              <div className={styles.hint}>
                An empty value uses the profile's default pool. A team that needs a cell of its
                own names a pool whose maxTenantsPerCell is 1.
              </div>
            </div>

            <div className={styles.field}>
              <label>Agent namespace</label>
              <select
                value={form.pipelinesAgentNamespace}
                onChange={(e) => updateField("pipelinesAgentNamespace", e.target.value)}
              >
                <option value="">Select a namespace</option>
                {form.namespaces
                  .map((n) => n.trim())
                  .filter(Boolean)
                  .map((n) => (
                    <option key={n} value={n}>
                      {n}
                    </option>
                  ))}
              </select>
              <div className={styles.hint}>Chosen from this team's own namespaces.</div>
            </div>

            <div className={styles.field}>
              <label>Pipelines bundle</label>
              <div className={styles.bundlePicker}>
                <select value={bundleCluster} onChange={(e) => setBundleCluster(e.target.value)}>
                  <option value="">Select a cluster</option>
                  {(clusters ?? []).map((c) => (
                    <option key={c.name} value={c.name}>
                      {c.name}
                    </option>
                  ))}
                </select>
                <select
                  value={form.pipelinesBundleName ? `${form.pipelinesBundleNamespace}::${form.pipelinesBundleName}` : ""}
                  onChange={(e) => onBundleChange(e.target.value)}
                  disabled={!bundleCluster}
                >
                  <option value="">No bundle</option>
                  {bundles.map((b) => (
                    <option key={bundleOptionValue(b)} value={bundleOptionValue(b)}>
                      {b.metadata.namespace}/{b.metadata.name}
                    </option>
                  ))}
                </select>
              </div>
              <div className={styles.hint}>Choosing a bundle records its namespace explicitly.</div>
            </div>

            <div className={styles.field}>
              <label>Rehydrate builds</label>
              <input
                type="number"
                min={0}
                max={100}
                value={form.pipelinesRehydrateBuilds}
                onChange={(e) => updateField("pipelinesRehydrateBuilds", e.target.value)}
                placeholder="10"
              />
              <div className={styles.hint}>
                Newest completed builds to seed when this tenant's cell changes. Default 10, range 0-100.
              </div>
            </div>

            <div className={styles.field}>
              <label>Rehydrate deadline</label>
              <input
                type="text"
                value={form.pipelinesRehydrateDeadline}
                onChange={(e) => updateField("pipelinesRehydrateDeadline", e.target.value)}
                placeholder="30m"
              />
              <div className={styles.hint}>
                How long to keep retrying a deferred rehydration seed. Default 30m, range 1m-24h.
              </div>
            </div>

            <div className={styles.field}>
              <label>Interrupted run retry</label>
              <select
                value={form.pipelinesInterruptedRetry}
                onChange={(e) =>
                  updateField("pipelinesInterruptedRetry", e.target.value as "manual" | "automatic")
                }
              >
                <option value="manual">Manual</option>
                <option value="automatic">Automatic</option>
              </select>
            </div>

            <div className={styles.field}>
              <label>Concurrency</label>
              <select
                value={form.pipelinesConcurrency}
                onChange={(e) =>
                  updateField("pipelinesConcurrency", e.target.value as "allow" | "cancelInProgress")
                }
              >
                <option value="allow">Allow (run side by side)</option>
                <option value="cancelInProgress">Cancel in progress</option>
              </select>
            </div>

            <div className={styles.toggleRow}>
              <input
                type="checkbox"
                id="pipelinesCronBackstop"
                checked={form.pipelinesCronBackstop}
                onChange={(e) => updateField("pipelinesCronBackstop", e.target.checked)}
              />
              <label htmlFor="pipelinesCronBackstop">Backstop cron scheduler</label>
            </div>

            <div className={styles.field}>
              <label>Cron catch-up</label>
              <select
                value={form.pipelinesCronCatchUp}
                onChange={(e) => updateField("pipelinesCronCatchUp", e.target.value as "one" | "none")}
              >
                <option value="one">Fire newest missed occurrence</option>
                <option value="none">Fire nothing</option>
              </select>
            </div>

            <div className={styles.field}>
              <label>Cron grace</label>
              <input
                type="text"
                value={form.pipelinesCronGrace}
                onChange={(e) => updateField("pipelinesCronGrace", e.target.value)}
                placeholder="5m"
              />
              <div className={styles.hint}>
                How long the backstop waits past a missed cron occurrence before it fires. Default 5m, range 1m-60m.
              </div>
            </div>

            <div className={styles.field}>
              <label>Exports (grants to other tenants)</label>
              <div className={styles.arrayEditor}>
                {form.pipelinesExports.map((exp, i) => (
                  <div key={i} className={styles.exportEntry}>
                    <div className={styles.exportEntryHead}>
                      <select
                        value={exp.kind}
                        onChange={(e) => updateExportKind(i, e.target.value as "job" | "locks")}
                      >
                        <option value="job">Job pattern</option>
                        <option value="locks">Lock patterns</option>
                      </select>
                      <button className={styles.removeBtn} onClick={() => removeExport(i)}>
                        ×
                      </button>
                    </div>

                    {exp.kind === "job" ? (
                      <input
                        type="text"
                        value={exp.job}
                        onChange={(e) => updateExportJob(i, e.target.value)}
                        placeholder="release/*"
                      />
                    ) : (
                      <div className={styles.arrayEditor}>
                        {exp.locks.map((lock, li) => (
                          <div key={li} className={styles.arrayRow}>
                            <input
                              type="text"
                              value={lock}
                              onChange={(e) => updateExportLock(i, li, e.target.value)}
                              placeholder="deploy-prod"
                            />
                            <button className={styles.removeBtn} onClick={() => removeExportLock(i, li)}>
                              ×
                            </button>
                          </div>
                        ))}
                        <button className={styles.addBtn} onClick={() => addExportLock(i)}>
                          + Add lock pattern
                        </button>
                      </div>
                    )}

                    <div className={styles.arrayEditor}>
                      <span className={styles.exportSubLabel}>Callers</span>
                      {exp.callers.map((caller, ci) => (
                        <div key={ci} className={styles.arrayRow}>
                          <input
                            type="text"
                            value={caller}
                            onChange={(e) => updateExportCaller(i, ci, e.target.value)}
                            placeholder="Team name"
                          />
                          <button className={styles.removeBtn} onClick={() => removeExportCaller(i, ci)}>
                            ×
                          </button>
                        </div>
                      ))}
                      <button className={styles.addBtn} onClick={() => addExportCaller(i)}>
                        + Add caller
                      </button>
                    </div>
                  </div>
                ))}
                <button className={styles.addBtn} onClick={addExport}>
                  + Add export
                </button>
              </div>
              <div className={styles.hint}>
                Grants of this tenant's jobs and locks to other tenants. An absent list exports
                nothing.
              </div>
            </div>
          </div>
        )}

        {isEdit && team && (team.pipelines || team.placement || placedCond || warningsCond || rehydratedCond) && (
          <Card title="Pipelines status">
            <div className={styles.statusGrid}>
              <div>
                <span className={styles.statusLabel}>Placement status</span>
                {placedCond ? (
                  <span
                    className={`${styles.statusBadge} ${
                      placedCond.status === "True" ? styles.statusReady : styles.statusNotReady
                    }`}
                  >
                    {placedCond.status === "True" ? "Placed" : placedCond.reason || "Unknown"}
                  </span>
                ) : (
                  <span className={styles.statusMuted}>Not yet placed</span>
                )}
              </div>

              <div>
                <span className={styles.statusLabel}>Cell</span>
                <span>
                  {team.placement?.cell
                    ? `${team.placement.cluster}/${team.placement.cellNamespace}/${team.placement.cell} (generation ${
                        team.placement.generation ?? 0
                      })`
                    : "—"}
                </span>
              </div>

              {team.placement?.members && team.placement.members.length > 0 && (
                <div>
                  <span className={styles.statusLabel}>Members</span>
                  <span>
                    {team.placement.members.map((member) => teamMemberLabel(member)).join(", ")}
                  </span>
                </div>
              )}

              <div>
                <span className={styles.statusLabel}>Rehydration status</span>
                {rehydratedCond ? (
                  <span
                    className={`${styles.statusBadge} ${
                      rehydratedCond.status === "True" ? styles.statusReady : styles.statusNotReady
                    }`}
                  >
                    {rehydratedCond.status === "True" ? "Rehydrated" : rehydratedCond.reason || "Unknown"}
                  </span>
                ) : (
                  <span className={styles.statusMuted}>No rehydration recorded</span>
                )}
              </div>

              {team.rehydration && (
                <div>
                  <span className={styles.statusLabel}>Rehydration records</span>
                  <span>
                    {team.rehydration.records ?? 0} record(s)
                    {team.rehydration.truncated ? " (truncated)" : ""}
                    {team.rehydration.unseededJobs ? `, ${team.rehydration.unseededJobs} job(s) unseeded` : ""}
                  </span>
                </div>
              )}

              {warningsCond?.status === "True" && (
                <div className={styles.warningBox}>
                  <span className={styles.statusLabel}>Tenant warnings</span>
                  <div className={styles.warningReason}>{warningsCond.reason}</div>
                  {warningsCond.message && <p className={styles.warningMessage}>{warningsCond.message}</p>}
                </div>
              )}
            </div>
          </Card>
        )}

        <div className={styles.formActions}>
          <Button onClick={handleSubmit} disabled={saving}>
            {saving ? "Saving..." : isEdit ? "Update" : "Create"}
          </Button>
          <Button onClick={() => navigate("/access/teams")}>Cancel</Button>
        </div>
      </div>
    </div>
  );
}
