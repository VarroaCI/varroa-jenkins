import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createApiKey, listApiKeys, revokeApiKey, rotateApiKey } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import LoadingSpinner from "../components/LoadingSpinner";
import { useToast } from "../components/Toast";
import type { KeyMeta } from "../types";
import styles from "./ApiKeys.module.css";

const dateFormatter = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" });

function parsedTime(value: string | undefined): number | null {
  if (!value) return null;
  const time = Date.parse(value);
  return Number.isNaN(time) ? null : time;
}

function DateValue({ value, fallback }: { value?: string; fallback: string }) {
  const time = parsedTime(value);
  if (time === null) return fallback;
  const full = new Date(time).toISOString();
  return <time dateTime={full} title={full}>{dateFormatter.format(time)}</time>;
}

export default function ApiKeys({ now = Date.now }: { now?: () => number }) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [secret, setSecret] = useState<string | null>(null);
  const [keyName, setKeyName] = useState("");
  const [revokeTarget, setRevokeTarget] = useState<string | null>(null);
  const createButtonRef = useRef<HTMLButtonElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const revokeTriggers = useRef(new Map<string, HTMLButtonElement>());

  const query = useQuery({ queryKey: ["apikeys"], queryFn: listApiKeys });
  const keys = query.data?.items ?? [];

  useEffect(() => {
    if (revokeTarget) cancelRef.current?.focus();
  }, [revokeTarget]);

  const createMutation = useMutation({
    mutationFn: () => createApiKey(undefined, keyName || undefined),
    onSuccess: (result) => {
      setSecret(result.token);
      setKeyName("");
      queryClient.invalidateQueries({ queryKey: ["apikeys"] });
    },
  });

  const rotateMutation = useMutation({
    mutationFn: (prefix: string) => rotateApiKey(prefix),
    onSuccess: (result) => {
      setSecret(result.token);
      queryClient.invalidateQueries({ queryKey: ["apikeys"] });
    },
  });

  const revokeMutation = useMutation({
    mutationFn: (prefix: string) => revokeApiKey(prefix),
    onSuccess: async (_, prefix) => {
      const index = keys.findIndex((key) => key.prefix === prefix);
      const nextPrefix = keys[index + 1]?.prefix ?? keys[index - 1]?.prefix;
      setRevokeTarget(null);
      await queryClient.invalidateQueries({ queryKey: ["apikeys"] });
      (nextPrefix ? revokeTriggers.current.get(nextPrefix) : createButtonRef.current)?.focus();
      toast("Key revoked");
    },
  });

  function closeRevoke() {
    const trigger = revokeTarget ? revokeTriggers.current.get(revokeTarget) : null;
    setRevokeTarget(null);
    requestAnimationFrame(() => trigger?.focus());
  }

  async function copySecret() {
    if (!secret) return;
    try {
      await navigator.clipboard.writeText(secret);
      toast("Token copied to clipboard");
    } catch {
      toast("Copy failed - select and copy manually");
    }
  }

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <div>
          <h1>API Keys</h1>
          <p>Create and manage long-lived credentials for automation. Keys carry your current permissions.</p>
        </div>
      </header>

      {secret && (
        <Card>
          <div className={styles.secret}>
            <strong>Copy this token now. It will not be shown again.</strong>
            <code>{secret}</code>
            <div className={styles.actions}>
              <Button variant="primary" onClick={copySecret}>Copy to clipboard</Button>
              <Button variant="ghost" onClick={() => setSecret(null)}>Dismiss</Button>
            </div>
          </div>
        </Card>
      )}

      <Card>
        <form className={styles.create} onSubmit={(event) => { event.preventDefault(); createMutation.mutate(); }}>
          <label htmlFor="api-key-name">Key name <span>(optional)</span></label>
          <div className={styles.createControls}>
            <input id="api-key-name" value={keyName} onChange={(event) => setKeyName(event.target.value)} placeholder="For example, release pipeline" />
            <Button ref={createButtonRef} variant="primary" type="submit" disabled={createMutation.isPending}>
              {createMutation.isPending ? "Generating..." : "Create API key"}
            </Button>
          </div>
          {createMutation.isError && <p className={styles.error} role="alert">Failed to create key. Check your connection and try again.</p>}
        </form>
      </Card>

      {query.isLoading && <LoadingSpinner />}
      {query.isError && (
        <div className={styles.state} role="alert">
          <strong>API keys could not be loaded.</strong>
          <span>Your existing keys have not been changed.</span>
          <Button onClick={() => query.refetch()} disabled={query.isFetching}>{query.isFetching ? "Retrying..." : "Retry"}</Button>
        </div>
      )}
      {!query.isLoading && !query.isError && keys.length === 0 && (
        <div className={styles.state}>
          <strong>No API keys yet</strong>
          <span>Create a key for a CLI, script, or CI/CD pipeline.</span>
          <Button onClick={() => createButtonRef.current?.focus()}>Create API key</Button>
        </div>
      )}
      {!query.isLoading && !query.isError && keys.length > 0 && (
        <div className={styles.tableWrap}>
          <table className={styles.table}>
            <thead><tr><th>Name</th><th>Prefix</th><th>Created</th><th>Last used</th><th>Expires</th><th>Status</th><th>Actions</th></tr></thead>
            <tbody>{keys.map((key: KeyMeta) => {
              const expiry = parsedTime(key.expires);
              const expired = expiry !== null && expiry <= now();
              return <tr key={key.prefix}>
                <td data-label="Name" className={styles.name}>{key.name?.trim() || "Unnamed key"}</td>
                <td data-label="Prefix"><code className={styles.prefix}>{key.prefix}</code></td>
                <td data-label="Created"><DateValue value={key.created} fallback="Unknown" /></td>
                <td data-label="Last used"><DateValue value={key.lastUsed} fallback="Never" /></td>
                <td data-label="Expires"><DateValue value={key.expires} fallback="Never expires" /></td>
                <td data-label="Status"><span className={`${styles.status} ${expired ? styles.expired : styles.active}`}>{expired ? "Expired" : "Active"}</span></td>
                <td data-label="Actions"><div className={styles.actions}>
                  <Button size="sm" variant="ghost" onClick={() => rotateMutation.mutate(key.prefix)} disabled={rotateMutation.isPending}>
                    {rotateMutation.isPending && rotateMutation.variables === key.prefix ? "Rotating..." : "Rotate"}
                  </Button>
                  <Button ref={(node) => { if (node) revokeTriggers.current.set(key.prefix, node); else revokeTriggers.current.delete(key.prefix); }} size="sm" variant="ghost" onClick={() => { revokeMutation.reset(); setRevokeTarget(key.prefix); }}>Revoke</Button>
                </div></td>
              </tr>;
            })}</tbody>
          </table>
          {rotateMutation.isError && <p className={styles.error} role="alert">Failed to rotate key. The existing key remains active.</p>}
        </div>
      )}

      {revokeTarget && (
        <div className={styles.backdrop} onMouseDown={(event) => { if (event.target === event.currentTarget && !revokeMutation.isPending) closeRevoke(); }}>
          <div className={styles.dialog} role="dialog" aria-modal="true" aria-labelledby="revoke-title" onKeyDown={(event) => { if (event.key === "Escape" && !revokeMutation.isPending) closeRevoke(); }}>
            <h2 id="revoke-title">Revoke API key?</h2>
            <p>Key <code>{revokeTarget}</code> will stop working immediately. Revocation is permanent.</p>
            {revokeMutation.isError && <p className={styles.error} role="alert">Failed to revoke key. The key remains active.</p>}
            <div className={styles.dialogActions}>
              <Button ref={cancelRef} onClick={closeRevoke} disabled={revokeMutation.isPending}>Cancel</Button>
              <Button variant="primary" onClick={() => revokeMutation.mutate(revokeTarget)} disabled={revokeMutation.isPending}>{revokeMutation.isPending ? "Revoking..." : "Revoke"}</Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
