import { useState, useCallback, useEffect } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  listUsers, createUser, deleteUser, updateUser, adminResetPassword,
  adminListUserApiKeys, adminRevokeApiKey,
  listRoleBindings, listJenkinsRoleBindings,
  listGroups, createGroup,
  type UserEntry, type GroupEntry,
} from "../api/client";
import { useAuth } from "../context/AuthContext";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import LoadingSpinner from "../components/LoadingSpinner";
import type { KeyMeta } from "../types";
import styles from "./settings.module.css";

// applyMembership upserts the groups whose membership for `username` changed to
// match `desired`. Returns the names of groups that failed to update. Group
// membership is canonical in the Group CRD, so we read-modify-write each.
async function applyMembership(groups: GroupEntry[], username: string, desired: Set<string>): Promise<string[]> {
  const failed: string[] = [];
  for (const g of groups) {
    const isMember = g.members.includes(username);
    const shouldBe = desired.has(g.name);
    if (isMember === shouldBe) continue;
    const members = shouldBe
      ? [...g.members, username]
      : g.members.filter((m) => m !== username);
    try {
      await createGroup({ name: g.name, displayName: g.displayName, members });
    } catch {
      failed.push(g.name);
    }
  }
  return failed;
}

export default function SettingsUsersTab() {
  const { authMode } = useAuth();
  const isLocal = authMode === "local";
  const { data: users, isLoading, error } = useQuery({
    queryKey: ["admin-users"],
    queryFn: listUsers,
  });
  const { data: groups } = useQuery({ queryKey: ["admin-groups"], queryFn: listGroups, enabled: isLocal });
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<UserEntry | null>(null);
  const [deletePreview, setDeletePreview] = useState<{ removedFrom: string[]; empty: string[] } | null>(null);
  const [selectedUser, setSelectedUser] = useState<UserEntry | null>(null);

  if (isLoading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorMsg}>Failed to load users</div>;

  return (
    <div>
      <div className={styles.toolbar}>
        <p className={styles.muted}>
          {users?.length || 0} user{users?.length !== 1 ? "s" : ""}
          {!isLocal && " · read-only directory (OIDC mode)"}
        </p>
        {isLocal && <Button onClick={() => setShowCreate(true)}>+ New User</Button>}
      </div>

      {showCreate && isLocal && (
        <CreateUserForm
          groups={groups || []}
          onClose={() => setShowCreate(false)}
          onCreated={() => { setShowCreate(false); qc.invalidateQueries({ queryKey: ["admin-users"] }); qc.invalidateQueries({ queryKey: ["admin-groups"] }); }}
        />
      )}

      {deleteTarget && (
        <DeleteConfirmDialog
          user={deleteTarget}
          preview={deletePreview}
          onClose={() => { setDeleteTarget(null); setDeletePreview(null); }}
          onConfirmed={() => {
            qc.invalidateQueries({ queryKey: ["admin-users"] });
            setDeleteTarget(null); setDeletePreview(null);
          }}
        />
      )}

      <table className={styles.table}>
        <thead>
          <tr>
            <th>Name</th><th>Email</th><th>Display Name</th><th>Groups</th><th>Last Login</th><th>Managed By</th><th></th>
          </tr>
        </thead>
        <tbody>
          {users?.map((u) => (
            <tr key={u.name}>
              <td>
                <Button variant="ghost" size="sm" className={`${styles.linkBtn} ${styles.profileVersion}`} onClick={() => setSelectedUser(selectedUser?.name === u.name ? null : u)}>
                  {u.name}
                </Button>
              </td>
              <td>{u.email || "—"}</td><td>{u.displayName || "—"}</td><td>{u.groups?.join(", ") || "—"}</td><td>{u.lastLogin ? new Date(u.lastLogin).toLocaleDateString() : "Never"}</td><td>{u.managedBy}</td>
              <td>
                <Button
                  variant="ghost"
                  size="sm"
                  className={`${styles.linkBtn} ${styles.dangerLink}`}
                  onClick={async () => {
                    setDeleteTarget(u);
                    try {
                      const vrbs = await listRoleBindings();
                      // Intentionally core-scoped: this is an advisory preview of
                      // bindings affected by deleting a user on the core-only user-
                      // management page. User identity and VarroaRoleBindings are
                      // core-side concepts, so no cluster selector belongs here.
                      const jrbs = await listJenkinsRoleBindings("core");
                      // Advisory preview: match by the identifiers a binding can
                      // reference (name/email). Display name is not an identifier.
                      const ids = new Set([u.name, u.email].filter(Boolean) as string[]);
                      const removed: string[] = [];
                      const empty: string[] = [];
                      for (const rb of vrbs.items) {
                        const hasUser = rb.spec.subjects?.some((s) => s.kind === "User" && ids.has(s.name));
                        if (hasUser) {
                          removed.push(`VarroaRoleBinding: ${rb.metadata.name}`);
                          if ((rb.spec.subjects?.length || 0) <= 1) empty.push(`VarroaRoleBinding: ${rb.metadata.name}`);
                        }
                      }
                      for (const rb of jrbs.items) {
                        const hasUser = rb.spec.subjects?.some((s) => s.kind === "User" && ids.has(s.name));
                        if (hasUser) {
                          removed.push(`JenkinsRoleBinding: ${rb.metadata.name}`);
                          if ((rb.spec.subjects?.length || 0) <= 1) empty.push(`JenkinsRoleBinding: ${rb.metadata.name}`);
                        }
                      }
                      setDeletePreview({ removedFrom: removed, empty });
                    } catch { setDeletePreview(null); }
                  }}
                >
                  Delete
                </Button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {users?.length === 0 && (
        <p className={styles.noResults}>No users found.</p>
      )}

      {selectedUser && (
        <div className={styles.userDetails}>
          {isLocal && (
            <div className={styles.editPanel}>
              <EditUserForm
                key={selectedUser.name}
                user={selectedUser}
                groups={groups || []}
                onSaved={() => qc.invalidateQueries({ queryKey: ["admin-users"] })}
              />
            </div>
          )}
          <Card title={`API Keys — ${selectedUser.name}`}>
            <PerUserApiKeys user={selectedUser.name} />
          </Card>
          {isLocal && (
            <div className={styles.stack12}>
              <ResetPasswordForm user={selectedUser.name} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// ---- Group checkbox selector ----

function GroupCheckboxes({ groups, selected, onToggle }: { groups: GroupEntry[]; selected: Set<string>; onToggle: (name: string) => void }) {
  if (groups.length === 0) return <p className={styles.smallMuted}>No groups defined.</p>;
  return (
    <div className={styles.checkboxes}>
      {groups.map((g) => (
        <label key={g.name} className={styles.checkboxLabel}>
          <input type="checkbox" checked={selected.has(g.name)} onChange={() => onToggle(g.name)} />
          {g.displayName || g.name}
        </label>
      ))}
    </div>
  );
}

// ---- Create User Form ----

function CreateUserForm({ groups, onClose, onCreated }: { groups: GroupEntry[]; onClose: () => void; onCreated: () => void }) {
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [error, setError] = useState("");

  const mutation = useMutation({
    mutationFn: async () => {
      await createUser({ username, email, displayName, password });
      // Apply initial group memberships via create-then-patch.
      const failed = await applyMembership(groups, username, selected);
      if (failed.length > 0) throw new Error(`User created, but failed to add to groups: ${failed.join(", ")}`);
    },
    onSuccess: onCreated,
    onError: (e: Error) => setError(e.message),
  });

  const toggle = (name: string) => setSelected((prev) => {
    const next = new Set(prev);
    if (next.has(name)) next.delete(name); else next.add(name);
    return next;
  });

  return (
    <div className={styles.stack16}>
      <Card title="Create User">
        <form onSubmit={(e) => { e.preventDefault(); setError(""); mutation.mutate(); }}>
          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Username</label>
            <input className={styles.formInput} value={username} onChange={(e) => setUsername(e.target.value)} placeholder="alice" required />
          </div>
          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Email</label>
            <input className={styles.formInput} type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="alice@example.com" />
          </div>
          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Display Name</label>
            <input className={styles.formInput} value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="Alice" />
          </div>
          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Password</label>
            <input className={styles.formInput} type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="Min 8 characters" required />
          </div>
          <div className={styles.formGroup}>
            <label className={styles.formLabel}>Groups</label>
            <GroupCheckboxes groups={groups} selected={selected} onToggle={toggle} />
          </div>
          {error && <p className={styles.formError}>{error}</p>}
          <div className={styles.inlineActions}>
            <Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? "Creating..." : "Create"}</Button>
            <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
          </div>
        </form>
      </Card>
    </div>
  );
}

// ---- Edit User Form (display name, email, group membership) ----

function EditUserForm({ user, groups, onSaved }: { user: UserEntry; groups: GroupEntry[]; onSaved: () => void }) {
  const [email, setEmail] = useState(user.email || "");
  const [displayName, setDisplayName] = useState(user.displayName || "");
  const [selected, setSelected] = useState<Set<string>>(new Set(user.groups || []));
  const [error, setError] = useState("");
  const [ok, setOk] = useState(false);

  const mutation = useMutation({
    mutationFn: async () => {
      await updateUser(user.name, { email, displayName });
      const failed = await applyMembership(groups, user.name, selected);
      if (failed.length > 0) throw new Error(`Saved, but these groups failed: ${failed.join(", ")}`);
    },
    onSuccess: () => { setOk(true); onSaved(); },
    onError: (e: Error) => setError(e.message),
  });

  const toggle = (name: string) => setSelected((prev) => {
    const next = new Set(prev);
    if (next.has(name)) next.delete(name); else next.add(name);
    return next;
  });

  return (
    <Card title={`Edit — ${user.name}`}>
      <form onSubmit={(e) => { e.preventDefault(); setError(""); setOk(false); mutation.mutate(); }}>
        <div className={styles.formGroup}>
          <label className={styles.formLabel}>Email</label>
          <input className={styles.formInput} type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
        </div>
        <div className={styles.formGroup}>
          <label className={styles.formLabel}>Display Name</label>
          <input className={styles.formInput} value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
        </div>
        <div className={styles.formGroup}>
          <label className={styles.formLabel}>Groups</label>
          <GroupCheckboxes groups={groups} selected={selected} onToggle={toggle} />
        </div>
        {error && <p className={styles.formError}>{error}</p>}
        {ok && <p className={styles.savedMessage}>Saved.</p>}
        <Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? "Saving..." : "Save changes"}</Button>
      </form>
    </Card>
  );
}

// ---- Delete Confirm Dialog ----

function DeleteConfirmDialog({ user, preview, onClose, onConfirmed }: {
  user: UserEntry; preview: { removedFrom: string[]; empty: string[] } | null; onClose: () => void; onConfirmed: () => void;
}) {
  const mutation = useMutation({
    mutationFn: () => deleteUser(user.name),
    onSuccess: onConfirmed,
  });

  return (
    <div className={styles.stack16}>
      <Card title={`Delete ${user.name}?`}>
        <p className={styles.muted}>
          This will deprovision the user across both RBAC planes and delete their User CRD. This action cannot be undone.
        </p>
        {preview && preview.removedFrom.length > 0 && (
          <div className={styles.deletePreview}>
            <div className={styles.previewHeading}><strong>Bindings this user is removed from:</strong></div>
            {preview.removedFrom.map((b) => <div key={b} className={styles.previewItem}>{b}</div>)}
            {preview.empty.length > 0 && (
              <>
                <div className={styles.previewDangerHeading}>
                  <strong>Bindings that will be deleted (become empty):</strong>
                </div>
                {preview.empty.map((b) => <div key={b} className={styles.previewDangerItem}>{b}</div>)}
              </>
            )}
          </div>
        )}
        <div className={styles.dialogActions}>
          <Button variant="ghost" onClick={() => mutation.mutate()} disabled={mutation.isPending}>
            {mutation.isPending ? "Deleting..." : "Delete"}
          </Button>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
        </div>
      </Card>
    </div>
  );
}

// ---- Per-User API Keys ----

function PerUserApiKeys({ user }: { user: string }) {
  const [keys, setKeys] = useState<KeyMeta[] | null>(null);
  const [loading, setLoading] = useState(false);

  const loadKeys = useCallback(async () => {
    setLoading(true);
    try { const result = await adminListUserApiKeys(user); setKeys(result.items); }
    catch { setKeys([]); }
    finally { setLoading(false); }
  }, [user]);

  useEffect(() => { loadKeys(); }, [loadKeys]);

  if (loading) return <LoadingSpinner />;
  if (keys === null) return <Button variant="ghost" onClick={loadKeys}>Load Keys</Button>;
  if (keys.length === 0) return <p className={styles.muted}>No API keys.</p>;

  return (
    <div>
      {keys.map((key) => (
        <div key={key.prefix} className={styles.apiKeyRow}>
          <div>
            <code className={styles.apiKeyCode}>{key.prefix}…</code>
            <div className={styles.apiKeyMeta}>
              Created: {key.created} · Expires: {key.expires || "Never"}
            </div>
          </div>
          <Button variant="ghost" size="sm" className={styles.dangerBtn} onClick={() => handleRevoke(key.prefix)}>Revoke</Button>
        </div>
      ))}
    </div>
  );

  async function handleRevoke(prefix: string) {
    await adminRevokeApiKey(user, prefix);
    setKeys((prev) => (prev ? prev.filter((k) => k.prefix !== prefix) : prev));
  }
}

// ---- Reset Password Form ----

function ResetPasswordForm({ user }: { user: string }) {
  const [pw, setPw] = useState("");
  const [error, setError] = useState("");
  const [ok, setOk] = useState(false);

  const mutation = useMutation({
    mutationFn: () => adminResetPassword(user, pw),
    onSuccess: () => { setOk(true); setPw(""); },
    onError: (e: Error) => setError(e.message),
  });

  return (
    <form onSubmit={(e) => { e.preventDefault(); setError(""); setOk(false); mutation.mutate(); }} className={styles.resetForm}>
      <input className={`${styles.formInput} ${styles.resetInput}`} type="password" value={pw} onChange={(e) => setPw(e.target.value)} placeholder="New password (min 8 chars)" />
      <Button className={styles.resetButton} type="submit" disabled={mutation.isPending}>Reset</Button>
      {error && <span className={styles.formErrorSmall}>{error}</span>}
      {ok && <span className={styles.savedSmall}>Password set.</span>}
    </form>
  );
}
