import { useState, type FormEvent } from "react";
import { useSearchParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { useCatalogSources } from "../hooks/useCatalog";
import { usePermissions, canDoGlobal, canDoInNamespace } from "../hooks/usePermissions";
import { createCatalogSource, updateCatalogSource, deleteCatalogSource, syncCatalogSource } from "../api/client";
import { useToast } from "../components/Toast";
import { Button } from "../components/Button";
import ClusterSelector from "../components/ClusterSelector";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import type { CatalogSource, CatalogSourceSpec, CatalogSyncPhase } from "../types";
import type { Permissions } from "../types/auth";
import styles from "./CatalogSources.module.css";

// Where a CatalogSource is published when the caller holds a global (not
// namespace-scoped) catalogsources:create capability — the "publish to the
// shared tier" case. Mirrors ComposerTray.tsx's DEFAULT_BUNDLE_NAMESPACE.
const DEFAULT_CATALOG_NAMESPACE = "varroa-system";

// Namespaces where a *non-selector*, explicitly-namespace-scoped grant exists
// for `verb`. Deliberately excludes hasControllerSelector scopes: the backend
// authorizer always calls EffectiveAPICapabilities with controllerName="" for
// CatalogSource, and resolver.go's scopeMatches unconditionally denies
// ControllerSelector scopes in that case — so a selector-scoped grant can
// never actually authorize a CatalogSource write server-side.
function scopedCatalogNamespaces(perms: Permissions | undefined, verb: string): string[] {
  return Array.from(
    new Set(
      (perms?.scopes ?? [])
        .filter((s) => !s.hasControllerSelector && (s.namespaces?.length ?? 0) > 0)
        .filter((s) => canDoInNamespace(perms, s.namespaces[0], "catalogsources", verb))
        .flatMap((s) => s.namespaces)
    )
  );
}

// Single source of truth for "may this caller act on a CatalogSource in ns",
// safe against the selector-scope leak described above. Used by both the
// namespace picker and every per-row gate below.
function canManageCatalogSourceIn(perms: Permissions | undefined, ns: string, verb: string): boolean {
  return canDoGlobal(perms, "catalogsources", verb) || scopedCatalogNamespaces(perms, verb).includes(ns);
}

interface DialogState {
  open: "create" | "edit" | "delete";
  source?: CatalogSource;
}

const EMPTY_SPEC: CatalogSourceSpec = {
  repoURL: "",
  revision: "",
  path: "",
  syncIntervalSeconds: 300,
  secretRef: "",
  trusted: false,
};

function PhaseBadge({ phase }: { phase?: CatalogSyncPhase }) {
  if (!phase) return null;
  const cls = [
    styles.phaseBadge,
    phase === "Ready" ? styles.phaseReady : "",
    phase === "Syncing" || phase === "Pending" ? styles.phaseSyncing : "",
    phase === "Error" ? styles.phaseError : "",
    phase === "Pending" ? styles.phasePending : "",
  ]
    .filter(Boolean)
    .join(" ");
  return <span className={cls}>{phase}</span>;
}

function SourceForm({
  spec,
  onChange,
}: {
  spec: CatalogSourceSpec;
  onChange: (s: CatalogSourceSpec) => void;
}) {
  function setField(field: keyof CatalogSourceSpec, type: "string" | "number" | "boolean" = "string") {
    return (e: React.ChangeEvent<HTMLInputElement>) => {
      const val = type === "number" ? Number(e.target.value) : type === "boolean" ? e.target.checked : e.target.value;
      onChange({ ...spec, [field]: val });
    };
  }

  return (
    <div>
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Repository URL *</label>
        <input
          className={styles.formInput}
          value={spec.repoURL}
          onChange={setField("repoURL")}
          placeholder="https://github.com/org/repo"
        />
      </div>
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Revision</label>
        <input
          className={styles.formInput}
          value={spec.revision || ""}
          onChange={setField("revision")}
          placeholder="main (default)"
        />
      </div>
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Path</label>
        <input
          className={styles.formInput}
          value={spec.path || ""}
          onChange={setField("path")}
          placeholder="/ (root)"
        />
      </div>
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Sync Interval (seconds)</label>
        <input
          className={styles.formInput}
          type="number"
          value={spec.syncIntervalSeconds ?? 300}
          onChange={setField("syncIntervalSeconds", "number")}
        />
      </div>
      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Secret Reference</label>
        <input
          className={styles.formInput}
          value={spec.secretRef || ""}
          onChange={setField("secretRef")}
          placeholder="Optional git auth secret"
        />
      </div>
      <div className={styles.formGroup}>
        <label className={styles.formCheckbox}>
          <input
            type="checkbox"
            checked={spec.trusted || false}
            onChange={setField("trusted", "boolean")}
          />
          Trusted source
        </label>
      </div>
    </div>
  );
}

export default function CatalogSources() {
  const queryClient = useQueryClient();
  const { data: perms } = usePermissions();
  const [searchParams, setSearchParams] = useSearchParams();
  const { cluster, ready } = useConfigurationCluster();
  const { data, isLoading, error } = useCatalogSources(cluster);
  const { toast } = useToast();

  const [dialog, setDialog] = useState<DialogState | null>(null);
  const [formSpec, setFormSpec] = useState<CatalogSourceSpec>({ ...EMPTY_SPEC });
  const [namespace, setNamespace] = useState<string>("");
  const [saving, setSaving] = useState(false);

  const sources = data?.items ?? [];

  const globalCreate = canDoGlobal(perms, "catalogsources", "create");
  const scopedCreateNamespaces = scopedCatalogNamespaces(perms, "create");

  function openCreate() {
    setFormSpec({ ...EMPTY_SPEC });
    setNamespace(globalCreate ? DEFAULT_CATALOG_NAMESPACE : (scopedCreateNamespaces[0] ?? ""));
    setDialog({ open: "create" });
  }

  function openEdit(source: CatalogSource) {
    setFormSpec({ ...source.spec });
    setDialog({ open: "edit", source });
  }

  function openDelete(source: CatalogSource) {
    setDialog({ open: "delete", source });
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!dialog) return;
    setSaving(true);
    try {
      if (dialog.open === "create") {
        await createCatalogSource(cluster!, namespace || DEFAULT_CATALOG_NAMESPACE, {
          apiVersion: "varroa.dev/v1alpha1",
          kind: "CatalogSource",
          metadata: { name: "" },
          spec: formSpec,
        });
        toast("Catalog source created");
      } else if (dialog.open === "edit" && dialog.source) {
        const ns = dialog.source.metadata.namespace || "default";
        const name = dialog.source.metadata.name;
        await updateCatalogSource(cluster!, ns, name, {
          ...dialog.source,
          spec: formSpec,
        });
        toast("Catalog source updated");
      }
      queryClient.invalidateQueries({ queryKey: ["catalog-sources"] });
      setDialog(null);
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete() {
    if (!dialog || dialog.open !== "delete" || !dialog.source) return;
    setSaving(true);
    try {
      const ns = dialog.source.metadata.namespace || "default";
      await deleteCatalogSource(cluster!, ns, dialog.source.metadata.name);
      queryClient.invalidateQueries({ queryKey: ["catalog-sources"] });
      toast("Catalog source deleted");
      setDialog(null);
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to delete");
    } finally {
      setSaving(false);
    }
  }

  async function handleSync(source: CatalogSource) {
    try {
      const ns = source.metadata.namespace || "default";
      await syncCatalogSource(cluster!, ns, source.metadata.name);
      toast("Sync triggered");
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to sync");
    }
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Catalog Sources</div>
          <div className={styles.pageDesc}>
            Manage repositories that provide catalog items
          </div>
        </div>
        <div style={{ display: "flex", alignItems: "flex-end", gap: 12 }}>
          {cluster && <ClusterSelector value={cluster} onChange={(value) => {
            const next = new URLSearchParams(searchParams);
            next.set("cluster", value);
            setSearchParams(next);
          }} />}
          {(globalCreate || scopedCreateNamespaces.length > 0) && (
            <Button variant="primary" onClick={openCreate}>
              + New Source
            </Button>
          )}
        </div>
      </div>

      {ready && !cluster && <NoAccessibleClusters />}
      {error && (
        <div className={styles.errorBanner}>
          Failed to load: {error.message}
        </div>
      )}

      {isLoading && <div className={styles.loadingBanner}>Loading catalog sources...</div>}

      {!isLoading && !error && sources.length === 0 && (
        <div className={styles.empty}>
          No catalog sources registered. Create one to get started.
        </div>
      )}

      {!isLoading && !error && sources.length > 0 && (
        <div className={styles.table}>
          <div className={styles.tableHeader}>
            <span>Name</span>
            <span>Repository</span>
            <span>Revision</span>
            <span>Status</span>
            <span>Items</span>
            <span>Trusted</span>
            <span />
          </div>
          {sources.map((source) => (
            <div key={source.metadata.name} className={styles.tableRow}>
              <span className={styles.cellName}>{source.metadata.name}</span>
              <span className={styles.cellMono}>
                {source.spec.repoURL}
              </span>
              <span className={styles.cellMono}>
                {source.spec.revision || "main"}
              </span>
              <span>
                <PhaseBadge phase={source.status?.phase} />
              </span>
              <span>{source.status?.itemCount ?? "-"}</span>
              <span>{source.spec.trusted ? "Yes" : "No"}</span>
              <div className={styles.actions}>
                {canManageCatalogSourceIn(perms, source.metadata.namespace ?? "default", "update") && (
                  <Button size="sm" variant="ghost" onClick={() => handleSync(source)}>
                    Sync
                  </Button>
                )}
                {canManageCatalogSourceIn(perms, source.metadata.namespace ?? "default", "update") && (
                  <Button size="sm" variant="ghost" onClick={() => openEdit(source)}>
                    Edit
                  </Button>
                )}
                {canManageCatalogSourceIn(perms, source.metadata.namespace ?? "default", "delete") && (
                  <Button size="sm" variant="ghost" onClick={() => openDelete(source)}>
                    Delete
                  </Button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Create / Edit dialog */}
      {(dialog?.open === "create" || dialog?.open === "edit") && (
        <div className={styles.overlay} onClick={() => setDialog(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>
              {dialog.open === "create" ? "New Catalog Source" : "Edit Catalog Source"}
            </div>
            <form onSubmit={handleSubmit}>
              {dialog.open === "create" && (
                <div className={styles.formGroup}>
                  <label className={styles.formLabel}>Namespace *</label>
                  {globalCreate ? (
                    <input
                      className={styles.formInput}
                      value={namespace}
                      onChange={(e) => setNamespace(e.target.value)}
                      placeholder={DEFAULT_CATALOG_NAMESPACE}
                    />
                  ) : (
                    <select
                      className={styles.formInput}
                      value={namespace}
                      onChange={(e) => setNamespace(e.target.value)}
                    >
                      {scopedCreateNamespaces.map((ns) => (
                        <option key={ns} value={ns}>
                          {ns}
                        </option>
                      ))}
                    </select>
                  )}
                </div>
              )}
              <SourceForm spec={formSpec} onChange={setFormSpec} />
              <div className={styles.dialogActions}>
                <Button type="button" onClick={() => setDialog(null)}>
                  Cancel
                </Button>
                <Button type="submit" variant="primary" disabled={saving || !formSpec.repoURL}>
                  {saving ? "Saving..." : "Save"}
                </Button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* Delete confirmation */}
      {dialog?.open === "delete" && dialog.source && (
        <div className={styles.overlay} onClick={() => setDialog(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>Delete Catalog Source</div>
            <p className={styles.dialogBody}>
              Are you sure you want to delete{" "}
              <b>{dialog.source.metadata.name}</b>?
              This will remove all items synced from this source.
            </p>
            <div className={styles.dialogActions}>
              <Button onClick={() => setDialog(null)}>Cancel</Button>
              <Button variant="primary" onClick={handleDelete} disabled={saving}>
                {saving ? "Deleting..." : "Delete"}
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
