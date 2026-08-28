import { useEffect, useState, type FormEvent } from "react";
import { getProvisioningDefaults, updateProvisioningDefaults } from "../api/client";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import type { ProvisioningDefaults } from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import ClusterSelector from "../components/ClusterSelector";
import { Button } from "../components/Button";
import styles from "./settings.module.css";

const EMPTY_ANNOTATION = { key: "", value: "" };
const EMPTY_PLUGIN = { name: "", version: "" };

export default function SettingsProvisioningTab() {
  const [config, setConfig] = useState<ProvisioningDefaults | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [selectedCluster, setSelectedCluster] = useState("core");
  const { permissions } = useAuth();
  const [annotations, setAnnotations] = useState<
    { key: string; value: string }[]
  >([{ ...EMPTY_ANNOTATION }]);
  const [plugins, setPlugins] = useState<{ name: string; version: string }[]>([
    { ...EMPTY_PLUGIN },
  ]);

  useEffect(() => {
    setConfig(null);
    setError(null);
    getProvisioningDefaults(selectedCluster)
      .then((cfg) => {
        setConfig(cfg);
        if (cfg.spec.ingressAnnotations) {
          const rows = Object.entries(cfg.spec.ingressAnnotations).map(
            ([key, value]) => ({ key, value }),
          );
          setAnnotations(rows.length > 0 ? rows : [{ ...EMPTY_ANNOTATION }]);
        }
        if (cfg.spec.defaultPlugins && cfg.spec.defaultPlugins.length > 0) {
          setPlugins(
            cfg.spec.defaultPlugins.map((p) => ({
              name: p.artifactId,
              version: p.version,
            })),
          );
        }
      })
      .catch((e) => setError(e.message));
  }, [selectedCluster]);

  if (error) return <div className={styles.errorMsg}>Error: {error}</div>;
  if (!config) return <LoadingSpinner />;

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
    try {
      const annMap: Record<string, string> = {};
      for (const a of annotations) {
        if (a.key.trim()) annMap[a.key.trim()] = a.value;
      }
      const plugEntries = plugins
        .filter((p) => p.name.trim())
        .map((p) => ({
          artifactId: p.name.trim(),
          version: p.version || "latest",
        }));
      const updated = {
        ...config,
        spec: {
          ...config.spec,
          ingressAnnotations: Object.keys(annMap).length > 0 ? annMap : undefined,
          defaultPlugins: plugEntries.length > 0 ? plugEntries : undefined,
          storageSizeGB: config.spec.storageSizeGB
            ? Number(config.spec.storageSizeGB)
            : undefined,
          provisioningTimeoutSec: config.spec.provisioningTimeoutSec
            ? Number(config.spec.provisioningTimeoutSec)
            : undefined,
        },
      };
      const cfg = await updateProvisioningDefaults(selectedCluster, updated);
      setConfig(cfg);
      setSaved(true);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  const spec = config.spec;
  const canSave = canDoGlobal(permissions, "provisioningdefaults", "update");

  return (
    <form onSubmit={handleSubmit}>
      <p className={`${styles.muted} ${styles.pageNote}`}>
        Defaults applied to controllers on the selected cluster unless overridden.
      </p>

      <ClusterSelector value={selectedCluster} onChange={setSelectedCluster} />

      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Root Domain</label>
        <input className={styles.formInput} value={spec.rootDomain || ""} onChange={setField("rootDomain")} placeholder="example.com" />
        <small className={styles.formHint}>Controllers without an ingress host will derive <code>&#123;name&#125;.example.com</code></small>
      </div>

      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Default Jenkins Version</label>
        <input className={styles.formInput} value={spec.defaultVersion || ""} onChange={setField("defaultVersion")} placeholder="latest" />
      </div>

      <div className={`${styles.formGroup} ${styles.row12}`}>
        <div className={styles.grow}>
          <label className={styles.formLabel}>Storage Class</label>
          <input className={styles.formInput} value={spec.storageClass || ""} onChange={setField("storageClass")} placeholder="standard" />
        </div>
        <div className={styles.grow}>
          <label className={styles.formLabel}>Storage Size (GB)</label>
          <input className={styles.formInput} type="number" value={spec.storageSizeGB ?? ""} onChange={setField("storageSizeGB")} placeholder="20" />
        </div>
      </div>

      <div className={styles.formGroup}>
        <label className={styles.formLabel}>Provisioning Timeout (seconds)</label>
        <input className={styles.formInput} type="number" value={spec.provisioningTimeoutSec ?? ""} onChange={setField("provisioningTimeoutSec")} placeholder="300" />
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

      <h3 className={styles.sectionTitle}>Default Plugins</h3>
      {plugins.map((p, i) => (
        <div key={i} className={`${styles.formGroup} ${styles.pluginRow}`}>
          <input className={`${styles.formInput} ${styles.pluginName}`} placeholder="artifactId" value={p.name} onChange={(e) => { const next = [...plugins]; next[i] = { ...next[i], name: e.target.value }; setPlugins(next); }} />
          <input className={`${styles.formInput} ${styles.pluginVersion}`} placeholder="version" value={p.version} onChange={(e) => { const next = [...plugins]; next[i] = { ...next[i], version: e.target.value }; setPlugins(next); }} />
          <Button type="button" variant="ghost" size="sm" onClick={() => setPlugins((prev) => [...prev.slice(0, i), ...prev.slice(i + 1)])} className={styles.dangerBtn}>×</Button>
        </div>
      ))}
      <Button type="button" variant="ghost" onClick={() => setPlugins((prev) => [...prev, { ...EMPTY_PLUGIN }])} className={`${styles.backLink} ${styles.addAction}`}>+ Add Plugin</Button>

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
