import { useState, useEffect } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { getTeam, createTeam, updateTeam, listRoles } from "../api/client";
import type { TeamEntry } from "../api/client";
import { Button } from "../components/Button";
import styles from "./TeamForm.module.css";

interface SubjectRef {
  kind: "Group" | "User";
  name: string;
}

interface FormData {
  name: string;
  displayName: string;
  members: string[];
  subjects: SubjectRef[];
  namespaces: string[];
  roleRef: string;
  provisionNamespaces: boolean;
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
  };
}

export default function TeamForm() {
  const navigate = useNavigate();
  const { name } = useParams<{ name: string }>();
  const isEdit = !!name;

  const [form, setForm] = useState<FormData>(emptyForm);
  const [availableRoles, setAvailableRoles] = useState<string[]>(["developer", "viewer", "editor"]);
  const [loading, setLoading] = useState(isEdit);
  const [saving, setSaving] = useState(false);
  const [errors, setErrors] = useState<string[]>([]);

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
        .then((team) => {
          setForm(formFromTeam(team));
          setLoading(false);
        })
        .catch(() => {
          setErrors(["Team not found"]);
          setLoading(false);
        });
    }
  }, [isEdit, name]);

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

  const validate = (): string[] => {
    const errs: string[] = [];
    if (!isEdit && !form.name.trim()) errs.push("Name is required");
    const hasMembers = form.members.some((m) => m.trim());
    const hasSubjects = form.subjects.some((s) => s.name.trim());
    if (!hasMembers && !hasSubjects) errs.push("At least one member or subject is required");
    const hasNs = form.namespaces.some((n) => n.trim());
    if (!hasNs) errs.push("At least one namespace is required");
    if (form.roleRef === "admin") errs.push("Role 'admin' is not permitted");
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
