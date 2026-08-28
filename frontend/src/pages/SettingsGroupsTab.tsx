import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { listGroups, createGroup, deleteGroup, listUsers, type UserEntry, type GroupEntry } from "../api/client";
import { useAuth } from "../context/AuthContext";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import LoadingSpinner from "../components/LoadingSpinner";
import styles from "./settings.module.css";

export default function SettingsGroupsTab() {
  const { authMode } = useAuth();
  const isLocal = authMode === "local";

  if (!isLocal) return <OIDCGroupsView />;
  return <LocalGroupsView />;
}

// ---- Local mode: full CRUD over Group CRDs ----

function LocalGroupsView() {
  const { data: groups, isLoading, error } = useQuery({ queryKey: ["admin-groups"], queryFn: listGroups });
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [editGroup, setEditGroup] = useState<string | null>(null);
  const [searchParams] = useSearchParams();
  const query = (searchParams.get("query") || "").toLowerCase();
  const filteredGroups = groups?.filter((group) => group.name.toLowerCase().includes(query));

  if (isLoading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorMsg}>Failed to load groups</div>;

  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin-groups"] });

  return (
    <div>
      <div className={styles.toolbar}>
        <p className={styles.muted}>{groups?.length || 0} group{groups?.length !== 1 ? "s" : ""}</p>
        <Button onClick={() => setShowCreate(true)}>+ New Group</Button>
      </div>

      {showCreate && (
        <GroupForm
          title="Create Group"
          onClose={() => setShowCreate(false)}
          onSaved={() => { setShowCreate(false); invalidate(); }}
        />
      )}

      {filteredGroups?.map((g) => (
        <div key={g.name} className={styles.stack12}>
          <Card title={g.displayName || g.name}>
            <div className={styles.rowBetween}>
              <span className={styles.muted}>
                {g.members.length} member{g.members.length !== 1 ? "s" : ""}
              </span>
              <div className={styles.row}>
                <Button variant="ghost" size="sm" className={styles.linkBtn} onClick={() => setEditGroup(editGroup === g.name ? null : g.name)}>
                  {editGroup === g.name ? "Close" : "Edit members"}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  className={`${styles.linkBtn} ${styles.dangerLink}`}
                  onClick={async () => {
                    if (confirm(`Delete group "${g.name}"?`)) {
                      await deleteGroup(g.name);
                      invalidate();
                    }
                  }}
                >
                  Delete
                </Button>
              </div>
            </div>
            {editGroup === g.name ? (
              <div className={styles.stack12}>
                <GroupForm
                  title=""
                  existing={g}
                  onClose={() => setEditGroup(null)}
                  onSaved={() => { setEditGroup(null); invalidate(); }}
                />
              </div>
            ) : (
              <div className={styles.stack12}>
                {g.members.length > 0 ? (
                  <div className={styles.wrap}>
                    {g.members.map((m) => <span key={m} className={styles.memberChip}>{m}</span>)}
                  </div>
                ) : (
                  <p className={styles.smallMuted}>No members</p>
                )}
              </div>
            )}
          </Card>
        </div>
      ))}

      {filteredGroups?.length === 0 && (
        <p className={styles.noResults}>No groups found.</p>
      )}
    </div>
  );
}

// ---- OIDC mode: read-only group-by over observed memberships ----

function OIDCGroupsView() {
  const { data: users, isLoading, error } = useQuery({ queryKey: ["admin-users"], queryFn: listUsers });
  if (isLoading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorMsg}>Failed to load users</div>;
  return (
    <div>
      <p className={`${styles.muted} ${styles.stack16}`}>
        OIDC mode — groups are observed from identity-provider claims at login and are read-only.
      </p>
      <OIDCGroupBy users={users || []} />
    </div>
  );
}


// ---- Group create/edit form (upsert) ----

function GroupForm({ title, existing, onClose, onSaved }: { title: string; existing?: GroupEntry; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(existing?.name || "");
  const [displayName, setDisplayName] = useState(existing?.displayName || "");
  const [members, setMembers] = useState((existing?.members || []).join(", "));
  const [error, setError] = useState("");

  const mutation = useMutation({
    mutationFn: () => createGroup({
      name,
      displayName,
      members: members.split(",").map((s) => s.trim()).filter(Boolean),
    }),
    onSuccess: onSaved,
    onError: (e: Error) => setError(e.message),
  });

  const body = (
    <form onSubmit={(e) => { e.preventDefault(); mutation.mutate(); }}>
      {!existing && (
        <div className={styles.formGroup}>
          <label className={styles.formLabel}>Name</label>
          <input className={styles.formInput} value={name} onChange={(e) => setName(e.target.value)} placeholder="developers" required />
        </div>
      )}
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Display Name</label>
        <input className={styles.formInput} value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="Developers Group" />
      </div>
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Members (comma-separated usernames)</label>
        <input className={styles.formInput} value={members} onChange={(e) => setMembers(e.target.value)} placeholder="alice, bob, charlie" />
      </div>
      {error && <p className={styles.formError}>{error}</p>}
      <div className={styles.inlineActions}>
        <Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? "Saving..." : "Save"}</Button>
        <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
      </div>
    </form>
  );

  if (!title) return body;
  return <div className={styles.stack16}><Card title={title}>{body}</Card></div>;
}

// ---- OIDC Group-by (read-only) ----

function OIDCGroupBy({ users }: { users: UserEntry[] }) {
  const [searchParams] = useSearchParams();
  const query = (searchParams.get("query") || "").toLowerCase();
  const groupMap = new Map<string, string[]>();
  for (const u of users) {
    if (u.managedBy !== "idp") continue;
    for (const g of u.groups || []) {
      if (!groupMap.has(g)) groupMap.set(g, []);
      groupMap.get(g)!.push(u.name);
    }
  }
  const entries = Array.from(groupMap.entries()).filter(([name, members]) => members.length > 0 && name.toLowerCase().includes(query));

  if (entries.length === 0) return <p className={styles.muted}>No observed groups with members.</p>;

  return (
    <div>
      {entries.map(([group, members]) => (
        <div key={group} className={styles.roleRule}>
          <div className={styles.roleSectionTitle}>{group}</div>
          <div className={styles.wrap}>
            {members.map((m) => <span key={m} className={styles.memberChip}>{m}</span>)}
          </div>
        </div>
      ))}
    </div>
  );
}
