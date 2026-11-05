import { useEffect, useState, type FormEvent } from "react";
import { getProvisioningDefaults, updateProvisioningDefaults } from "../api/client";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import { ApiError } from "../hooks/useApi";
import type { ProvisioningDefaults, ProvisioningDefaultsSpec } from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import ClusterSelector from "../components/ClusterSelector";
import { Button } from "../components/Button";
import styles from "./settings.module.css";

const EMPTY_ANNOTATION = { key: "", value: "" };
const MISSING_NOTE = "This cluster has no provisioning defaults. Save creates them.";
const CONFLICT_NOTE = "Another user or tool changed these defaults after this page read them. Select Reload to read the new values, then make your change again. Reload discards your unsaved edits.";

function emptyDefaults(): ProvisioningDefaults {
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "ProvisioningDefaults",
    metadata: { name: "varroa-defaults" },
    spec: {},
  };
}

function annotationRows(spec: ProvisioningDefaultsSpec): { key: string; value: string }[] {
  const rows = Object.entries(spec.ingressAnnotations ?? {}).map(
    ([key, value]) => ({ key, value }),
  );
  return rows.length > 0 ? rows : [{ ...EMPTY_ANNOTATION }];
}

export default function SettingsProvisioningTab() {
  const [config, setConfig] = useState<ProvisioningDefaults | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [missing, setMissing] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);
  const [selectedCluster, setSelectedCluster] = useState("core");
  const { permissions } = useAuth();
  const [annotations, setAnnotations] = useState<
    { key: string; value: string }[]
  >([{ ...EMPTY_ANNOTATION }]);

  useEffect(() => {
    setConfig(null);
    setError(null);
    setSaveError(null);
    setSaved(false);
    setMissing(false);
    setConflict(false);
    getProvisioningDefaults(selectedCluster)
      .then((cfg) => {
        setConfig(cfg);
        setAnnotations(annotationRows(cfg.spec));
      })
      .catch((e) => {
        if (e instanceof ApiError && e.status === 404) {
          setConfig(emptyDefaults());
          setAnnotations([{ ...EMPTY_ANNOTATION }]);
          setMissing(true);
          return;
        }
        setError(e.message);
      });
  }, [selectedCluster, reloadKey]);

  // The intro and the cluster selector render in every state, so a user who
  // picked an unreachable cluster can switch back. The form root stays the
  // same element, which keeps the selector mounted across state changes.
  const header = (
    <>
      <p className={`${styles.muted} ${styles.pageNote}`}>
        Defaults applied to controllers on the selected cluster unless overridden.
      </p>

      <ClusterSelector value={selectedCluster} onChange={setSelectedCluster} />
    </>
  );

  if (error) {
    return (
      <form onSubmit={handleSubmit}>
        {header}
        <div className={styles.errorMsg}>Error: {error}</div>
      </form>
    );
  }
  if (!config) {
    return (
      <form onSubmit={handleSubmit}>
        {header}
        <LoadingSpinner />
      </form>
    );
  }

  function setField(field: string) {
    return (e: React.ChangeEvent<HTMLInputElement>) => {
      setConfig((prev) => {
        if (!prev) return prev;
        return { ...prev, spec: { ...prev.spec, [field]: e.target.value } };
      });
    };
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!config) return;
    setSaving(true);
    setSaved(false);
    setSaveError(null);
    setConflict(false);
    try {
      const annMap: Record<string, string> = {};
      for (const a of annotations) {
        if (a.key.trim()) annMap[a.key.trim()] = a.value;
      }
      const updated = {
        ...config,
        spec: {
          ...config.spec,
          ingressAnnotations: Object.keys(annMap).length > 0 ? annMap : undefined,
          provisioningTimeoutSec: config.spec.provisioningTimeoutSec
            ? Number(config.spec.provisioningTimeoutSec)
            : undefined,
        },
      };
      const cfg = await updateProvisioningDefaults(selectedCluster, updated);
      setConfig(cfg);
      setMissing(false);
      setSaved(true);
    } catch (e: unknown) {
      if (e instanceof ApiError && e.status === 409) {
        setConflict(true);
        return;
      }
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  const spec = config.spec;
  const canSave = canDoGlobal(permissions, "provisioningdefaults", "update");

  return (
    <form onSubmit={handleSubmit}>
      {header}
      {missing && <p className={`${styles.muted} ${styles.pageNote}`}>{MISSING_NOTE}</p>}

      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Root Domain</label>
        <input className={styles.formInput} value={spec.rootDomain || ""} onChange={setField("rootDomain")} placeholder="example.com" />
        <small className={styles.formHint}>Controllers without an ingress host will derive <code>&#123;name&#125;.example.com</code></small>
      </div>

      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Default Jenkins Version</label>
        <input className={styles.formInput} value={spec.defaultVersion || ""} onChange={setField("defaultVersion")} placeholder="latest" />
      </div>

      <div className={styles.formGroup}>
        <label className={styles.formLabel} htmlFor="storageClass">Storage Class</label>
        <input id="storageClass" className={styles.formInput} value={spec.storageClass || ""} onChange={setField("storageClass")} placeholder="standard" />
      </div>

      <div className={styles.formGroup}>
        <label className={styles.formLabel} htmlFor="provisioningTimeoutSec">Provisioning Timeout (seconds)</label>
        <input id="provisioningTimeoutSec" className={styles.formInput} type="number" min={60} max={86400} value={spec.provisioningTimeoutSec ?? ""} onChange={setField("provisioningTimeoutSec")} placeholder="300" />
        <small className={styles.formHint}>Image pulls and the plugin download do not count against this limit, up to 15 minutes. Empty means 300 seconds.</small>
      </div>

      <h3 className={styles.sectionTitle}>Default Resources</h3>
      <div className={`${styles.formGroup} ${styles.row12}`}>
        <div className={styles.grow}>
          <label className={styles.formLabel}>CPU</label>
          <input className={styles.formInput} value={spec.defaultCPU || ""} onChange={setField("defaultCPU")} placeholder="2" />
        </div>
        <div className={styles.grow}>
          <label className={styles.formLabel}>Memory</label>
          <input className={styles.formInput} value={spec.defaultMemory || ""} onChange={setField("defaultMemory")} placeholder="4Gi" />
        </div>
        <div className={styles.grow}>
          <label className={styles.formLabel}>Storage</label>
          <input className={styles.formInput} value={spec.defaultStorage || ""} onChange={setField("defaultStorage")} placeholder="20Gi" />
        </div>
      </div>

      <h3 className={styles.sectionTitle}>Ingress Annotations</h3>
      <small className={styles.formHint}>Use <code>{"{{.Name}}"}</code> and <code>{"{{.Namespace}}"}</code> for template variables.</small>
      {annotations.map((a, i) => (
        <div key={i} className={`${styles.formGroup} ${styles.annotationRow}`}>
          <input className={`${styles.formInput} ${styles.annotationKey}`} placeholder="annotation key" value={a.key} onChange={(e) => { const next = [...annotations]; next[i] = { ...next[i], key: e.target.value }; setAnnotations(next); }} />
          <input className={`${styles.formInput} ${styles.annotationValue}`} placeholder="value" value={a.value} onChange={(e) => { const next = [...annotations]; next[i] = { ...next[i], value: e.target.value }; setAnnotations(next); }} />
          <Button type="button" variant="ghost" size="sm" onClick={() => setAnnotations((prev) => [...prev.slice(0, i), ...prev.slice(i + 1)])} className={styles.dangerBtn}>×</Button>
        </div>
      ))}
      <Button type="button" variant="ghost" onClick={() => setAnnotations((prev) => [...prev, { ...EMPTY_ANNOTATION }])} className={`${styles.backLink} ${styles.addAction}`}>+ Add Annotation</Button>

      <div className={styles.formGroup}>
        {conflict && (
          <div role="alert" className={styles.errorMsg}>
            <p>{CONFLICT_NOTE}</p>
            <Button type="button" onClick={() => setReloadKey((k) => k + 1)}>Reload</Button>
          </div>
        )}
        {saveError && (
          <div role="alert" className={styles.errorMsg}>Error: {saveError}</div>
        )}
        {saved && <span className={styles.saved}>Saved.</span>}
        {canSave ? (
          <Button type="submit" variant="primary" disabled={saving}>{saving ? "Saving..." : "Save"}</Button>
        ) : (
          <p className={styles.permissionNote}>You don't have permission to modify settings.</p>
        )}
      </div>
    </form>
  );
}
