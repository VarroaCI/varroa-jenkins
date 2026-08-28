import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { useComposer, itemRefKey } from "../context/ComposerContext";
import { useControllers } from "../hooks/useControllers";
import { clusterQuery } from "../routing";
import { ApiError } from "../hooks/useApi";
import { previewComposedBundle, createComposedBundle, getComposedBundle, updateController, updateComposedBundle, validateComposedBundle } from "../api/client";
import { useToast } from "../components/Toast";
import { Button } from "../components/Button";
import { Tabs } from "../components/Tabs";
import type { ComposedBundlePreview } from "../types";
import type { EditTarget } from "./ComposedBundleEdit";
import styles from "./ComposerTray.module.css";

interface ComposerTrayProps {
  open: boolean;
  onClose: () => void;
  editTarget?: EditTarget;
}

// Where a composed bundle is created when no target controller is selected.
// When a target controller IS selected we create/preview/validate in that
// controller's namespace instead, because the backend resolves a controller's
// composedBundleRef in the controller's own namespace.
const DEFAULT_BUNDLE_NAMESPACE = "varroa-system";

const MERGE_STRATEGIES = [
  { value: "errorOnConflict", label: "Error on conflict" },
  { value: "override", label: "Override (last wins)" },
];

const PREVIEW_TABS = [
  { id: "bundle", label: <>Bundle</> },
  { id: "jenkins", label: <>Jenkins</> },
  { id: "plugins", label: <>Plugins</> },
  { id: "items", label: <>Items</> },
  { id: "rbac", label: <>RBAC</> },
];

const TYPE_LABELS: Record<string, string> = {
  podtemplate: "Pod Templates",
  plugin: "Plugins",
  item: "Items",
  jcasc: "JCasC",
  rbac: "RBAC",
};

/** Derive the ComposedBundle resource name from a display name. Used by both
 *  the create flow and edit-mode "Save as new" so the two can never disagree
 *  about which resource a display name maps to. */
function slugifyBundleName(displayName: string): string {
  return displayName
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");
}

export default function ComposerTray({ open, onClose, editTarget }: ComposerTrayProps) {
  const composer = useComposer();
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const { data: controllers } = useControllers();
  const navigate = useNavigate();

  // In edit mode, fall back to the bundle's resource name when the spec has no
  // displayName (e.g. bundles created via kubectl), so the required field is
  // never blank for an existing bundle.
  const [displayName, setDisplayName] = useState(
    editTarget ? editTarget.baseBundle.spec.displayName || editTarget.name : "",
  );
  const [description, setDescription] = useState(editTarget?.baseBundle.spec.description ?? "");
  const [mergeStrategy, setMergeStrategy] = useState(editTarget?.baseBundle.spec.jcascMergeStrategy ?? "errorOnConflict");
  const [targetController, setTargetController] = useState("");
  const [preview, setPreview] = useState<ComposedBundlePreview | null>(null);
  const [previewTab, setPreviewTab] = useState("bundle");
  const [previewing, setPreviewing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [saving, setSaving] = useState(false);

  // Validate state
  const [validating, setValidating] = useState(false);
  const [validationResult, setValidationResult] = useState<{
    valid: boolean;
    errors: string[];
    warnings: string[];
  } | null>(null);

  // Bundle namespace: the target controller's namespace when one is selected,
  // otherwise the default. This keeps preview/create/validate/attach consistent
  // and ensures an attached bundle lives where the controller looks for it.
  const bundleNamespace =
    controllers?.find((c) => c.name === targetController)?.namespace ??
    DEFAULT_BUNDLE_NAMESPACE;

  // The cluster this composer session authors against (seeded by the catalog
  // browser / bundle editor). All preview/create/update/validate calls are
  // addressed to it so config lands on the intended cluster, not always the core.
  const cluster = composer.cluster ?? "core";

  // Group items by type (we infer type from the item name since we don't have the full catalog item here)
  const groupedItems: Record<string, typeof composer.items> = {};
  for (const item of composer.items) {
    const type = item.name.includes("podtemplate")
      ? "podtemplate"
      : item.name.includes("plugin")
        ? "plugin"
        : item.name.includes("rbac")
          ? "rbac"
          : item.name.includes("jcasc") || item.name.includes("jenkins")
            ? "jcasc"
            : "item";
    if (!groupedItems[type]) groupedItems[type] = [];
    groupedItems[type].push(item);
  }

  async function handlePreview() {
    if (!displayName.trim()) {
      toast("Display name is required");
      return;
    }
    setPreviewing(true);
    setPreview(null);
    try {
      const spec = composer.toSpec(displayName.trim());
      const result = await previewComposedBundle(cluster, bundleNamespace, spec);
      setPreview(result);
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Preview failed");
    } finally {
      setPreviewing(false);
    }
  }

  async function handleCreate(attach: boolean) {
    if (!displayName.trim()) {
      toast("Display name is required");
      return;
    }
    setCreating(true);
    try {
      const spec = composer.toSpec(displayName.trim());
      spec.description = description.trim() || undefined;
      spec.jcascMergeStrategy = mergeStrategy;
      // The resource name is the slugified display name; the attach below must
      // reference this exact name, not the raw display name, or the backend
      // rejects the controller patch as "composedBundle not found".
      const bundleName = slugifyBundleName(displayName);
      await createComposedBundle(cluster, bundleNamespace, {
        apiVersion: "varroa.dev/v1alpha1",
        kind: "ComposedBundle",
        metadata: { name: bundleName },
        spec,
      });
      queryClient.invalidateQueries({ queryKey: ["composed-bundles"] });

      // If attach mode, also patch the target controller
      if (attach && targetController) {
        const ctrl = controllers?.find((c) => c.name === targetController);
        if (ctrl) {
          await updateController(ctrl.cluster, ctrl.name, ctrl.namespace, {
            spec: { composedBundleRef: { name: bundleName } },
          });
          queryClient.invalidateQueries({ queryKey: ["controllers"] });
          toast("Composed bundle created and attached to controller");
        } else {
          toast("Bundle created but controller not found for attach");
        }
      } else {
        toast("Composed bundle created");
      }

      handleClear();
      onClose();
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to create");
    } finally {
      setCreating(false);
    }
  }

  async function handleSave() {
    if (!editTarget) return;
    if (!displayName.trim()) {
      toast("Display name is required");
      return;
    }
    setSaving(true);
    try {
      const spec = composer.toSpec(displayName.trim());
      spec.description = description.trim() || undefined;
      spec.jcascMergeStrategy = mergeStrategy;
      if (editTarget.gitInputs.length > 0) {
        spec.inputs = [...(spec.inputs ?? []), ...editTarget.gitInputs.map((g) => ({ gitSource: g }))];
      }
      await updateComposedBundle(cluster, editTarget.namespace, editTarget.name, {
        ...editTarget.baseBundle,
        spec,
      });
      queryClient.invalidateQueries({ queryKey: ["composed-bundles"] });
      toast("Bundle updated");
      // Saved successfully — discard the persisted edit draft so a later edit
      // starts from the now-current bundle rather than these (now-applied) edits.
      composer.clearPersisted();
      navigate(`/catalog/bundles/${editTarget.namespace}/${editTarget.name}${clusterQuery(cluster)}`);
      onClose();
    } catch (e: unknown) {
      // bffFetch throws Error("<status> <statusText>: <body>"); a stale
      // resourceVersion update returns 409 Conflict.
      if (e instanceof ApiError && e.status === 409) {
        toast("Bundle changed by another user — reload and retry");
      } else {
        toast(e instanceof Error ? e.message : "Failed to update bundle");
      }
    } finally {
      setSaving(false);
    }
  }

  // "Save as new": create a fresh ComposedBundle (named from the edited display
  // name) in the edited bundle's namespace, leaving the original untouched.
  // POST /composedbundles has apply (create-or-update) semantics, so guard
  // against name collisions client-side — otherwise a "save as" would silently
  // overwrite an existing bundle.
  async function handleSaveAs() {
    if (!editTarget) return;
    if (!displayName.trim()) {
      toast("Display name is required");
      return;
    }
    const newName = slugifyBundleName(displayName);
    if (!newName) {
      toast("Display name must contain letters or numbers");
      return;
    }
    if (newName === editTarget.name) {
      toast("Change the display name to save as a new bundle");
      return;
    }
    setCreating(true);
    try {
      try {
        await getComposedBundle(cluster, editTarget.namespace, newName);
        toast(`A bundle named "${newName}" already exists`);
        return;
      } catch (e: unknown) {
        // Only a 404 means the name is free; any other failure is inconclusive
        // and proceeding could overwrite an existing bundle.
        if (!(e instanceof ApiError && e.status === 404)) throw e;
      }
      const spec = composer.toSpec(displayName.trim());
      spec.description = description.trim() || undefined;
      spec.jcascMergeStrategy = mergeStrategy;
      if (editTarget.gitInputs.length > 0) {
        spec.inputs = [...(spec.inputs ?? []), ...editTarget.gitInputs.map((g) => ({ gitSource: g }))];
      }
      await createComposedBundle(cluster, editTarget.namespace, {
        apiVersion: "varroa.dev/v1alpha1",
        kind: "ComposedBundle",
        metadata: { name: newName },
        spec,
      });
      queryClient.invalidateQueries({ queryKey: ["composed-bundles"] });
      toast(`Saved as new bundle "${newName}"`);
      // The edits now live in the new bundle; drop the original's edit draft.
      composer.clearPersisted();
      navigate(`/catalog/bundles/${editTarget.namespace}/${newName}${clusterQuery(cluster)}`);
      onClose();
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Failed to save as new bundle");
    } finally {
      setCreating(false);
    }
  }

  async function handleValidate() {
    if (!displayName.trim()) {
      toast("Display name is required");
      return;
    }
    setValidating(true);
    setValidationResult(null);
    try {
      const spec = composer.toSpec(displayName.trim());
      const result = await validateComposedBundle(cluster, bundleNamespace, spec);
      setValidationResult(result);
    } catch (e: unknown) {
      toast(e instanceof Error ? e.message : "Validation failed");
    } finally {
      setValidating(false);
    }
  }

  function handleClear() {
    composer.clear();
    setPreview(null);
    setDisplayName(editTarget ? editTarget.baseBundle.spec.displayName || editTarget.name : "");
    setDescription(editTarget?.baseBundle.spec.description ?? "");
    setMergeStrategy(editTarget?.baseBundle.spec.jcascMergeStrategy ?? "errorOnConflict");
    setTargetController("");
    setValidationResult(null);
  }

  function getPreviewYaml(): string {
    if (!preview) return "";
    switch (previewTab) {
      case "bundle":
        return preview.bundleYaml;
      case "jenkins":
        return preview.jenkinsYaml;
      case "plugins":
        return preview.pluginsYaml;
      case "items":
        return preview.itemsYaml;
      case "rbac":
        return preview.rbacYaml;
      default:
        return preview.bundleYaml;
    }
  }

  if (!open) return null;

  const saveDisabled = saving || (composer.items.length === 0 && (editTarget?.gitInputs.length ?? 0) === 0);

  return (
    <div className={styles.overlay} onClick={onClose}>
      <div className={styles.tray} onClick={(e) => e.stopPropagation()}>
        <div className={styles.trayHead}>
          <div className={styles.trayTitle}>Bundle Composer</div>
          <button className={styles.closeBtn} onClick={onClose}>
            &times;
          </button>
        </div>

        <div className={styles.trayBody}>
          {/* Form fields */}
          <div className={styles.formFields}>
            <div className={styles.formGroup}>
              <label className={styles.formLabel}>Display Name *</label>
              <input
                className={styles.formInput}
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                placeholder="My composed bundle"
              />
            </div>
            {!editTarget && (
              <div className={styles.formGroup}>
                <label className={styles.formLabel}>Target controller</label>
                <select
                  className={styles.formSelect}
                  value={targetController}
                  onChange={(e) => setTargetController(e.target.value)}
                >
                  <option value="">-- None (create only) --</option>
                  {controllers?.map((c) => (
                    <option key={c.name} value={c.name}>
                      {c.name} ({c.namespace})
                    </option>
                  ))}
                </select>
              </div>
            )}
            <div className={styles.formGroup}>
              <label className={styles.formLabel}>Description</label>
              <input
                className={styles.formInput}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="Optional description"
              />
            </div>
            <div className={styles.formGroup}>
              <label className={styles.formLabel}>JCasC Merge Strategy</label>
              <select
                className={styles.formSelect}
                value={mergeStrategy}
                onChange={(e) => setMergeStrategy(e.target.value)}
              >
                {MERGE_STRATEGIES.map((s) => (
                  <option key={s.value} value={s.value}>
                    {s.label}
                  </option>
                ))}
              </select>
            </div>
          </div>

          {/* Items list */}
          <div className={styles.itemsSection}>
            <div className={styles.itemsSectionTitle}>
              Items ({composer.items.length})
            </div>
            {composer.items.length === 0 ? (
              <div className={styles.emptyItems}>
                Add items from the catalog browser to compose a bundle.
              </div>
            ) : (
              Object.entries(groupedItems).map(([type, typeItems]) => (
                <div key={type} className={styles.group}>
                  <div className={styles.groupLabel}>
                    {TYPE_LABELS[type] || type}
                  </div>
                  {typeItems.map((item) => {
                    const globalIndex = composer.items.indexOf(item);
                    return (
                      <div key={itemRefKey(item)} className={styles.itemRow}>
                        <div className={styles.itemInfo}>
                          <div className={styles.itemName}>
                            {item.name}
                            {item.namespace && (
                              <span className={styles.nsBadge}>{item.namespace}</span>
                            )}
                          </div>
                        </div>
                        <div className={styles.itemActions}>
                          <button
                            className={styles.moveBtn}
                            disabled={globalIndex === 0}
                            onClick={() => composer.reorderItem(globalIndex, globalIndex - 1)}
                            title="Move up"
                          >
                            &#x25B2;
                          </button>
                          <button
                            className={styles.moveBtn}
                            disabled={globalIndex === composer.items.length - 1}
                            onClick={() => composer.reorderItem(globalIndex, globalIndex + 1)}
                            title="Move down"
                          >
                            &#x25BC;
                          </button>
                          <button
                            className={styles.removeBtn}
                            onClick={() => composer.removeItem(item)}
                            title="Remove"
                          >
                            &times;
                          </button>
                        </div>
                      </div>
                    );
                  })}
                </div>
              ))
            )}
          </div>

          {/* Preview section */}
          {preview && (
            <div className={styles.previewSection}>
              <div className={styles.previewHead}>
                <div className={styles.previewTitle}>Preview</div>
              </div>

              {/* Missing/drifted */}
              {preview.missing.length > 0 && (
                <div className={styles.warningBanner}>
                  <strong>Missing items:</strong> {preview.missing.join(", ")}
                </div>
              )}
              {preview.drifted.length > 0 && (
                <div className={styles.warningBanner}>
                  <strong>Drifted items:</strong> {preview.drifted.join(", ")}
                </div>
              )}
              {preview.warnings.length > 0 && (
                <div className={styles.warningsList}>
                  {preview.warnings.map((w, i) => (
                    <div key={i} className={styles.warningItem}>
                      &#x26A0; {w}
                    </div>
                  ))}
                </div>
              )}

              <Tabs tabs={PREVIEW_TABS} activeTab={previewTab} onSelect={setPreviewTab} />
              <pre className={styles.yamlBlock}>{getPreviewYaml()}</pre>
            </div>
          )}

          {/* Validation results */}
          {validationResult && (
            <div className={styles.validationSection}>
              <div className={styles.validationHead}>
                {validationResult.valid ? (
                  <span className={styles.validationOk}>&#10003; Valid</span>
                ) : (
                  <span className={styles.validationBad}>&#10007; Invalid</span>
                )}
              </div>
              {validationResult.errors.length > 0 && (
                <div className={styles.validationIssues}>
                  {validationResult.errors.map((err, i) => (
                    <div key={`verr-${i}`} className={styles.validationError}>⛔ {err}</div>
                  ))}
                </div>
              )}
              {validationResult.warnings.length > 0 && (
                <div className={styles.validationIssues}>
                  {validationResult.warnings.map((w, i) => (
                    <div key={`vwarn-${i}`} className={styles.validationWarning}>⚠ {w}</div>
                  ))}
                </div>
              )}
            </div>
          )}

          {/* Actions */}
          <div className={styles.actions}>
            <Button variant="ghost" onClick={handleClear} disabled={composer.items.length === 0}>
              Clear
            </Button>
            <Button
              variant="ghost"
              onClick={handleValidate}
              disabled={validating || composer.items.length === 0}
            >
              {validating ? "Validating..." : "Validate"}
            </Button>
            <Button
              variant="ghost"
              onClick={handlePreview}
              disabled={previewing || composer.items.length === 0}
            >
              {previewing ? "Previewing..." : "View bundle.yaml"}
            </Button>
            {editTarget ? (
              <>
                <Button
                  variant="ghost"
                  onClick={handleSaveAs}
                  disabled={creating || saveDisabled}
                >
                  {creating ? "Saving..." : "Save as new"}
                </Button>
                <Button
                  variant="primary"
                  onClick={handleSave}
                  disabled={creating || saveDisabled}
                >
                  {saving ? "Saving..." : "Save changes"}
                </Button>
              </>
            ) : (
              <>
                <Button
                  variant="primary"
                  onClick={() => handleCreate(false)}
                  disabled={creating || composer.items.length === 0}
                >
                  {creating ? "Creating..." : "Create bundle"}
                </Button>
                {targetController && (
                  <Button
                    variant="primary"
                    onClick={() => handleCreate(true)}
                    disabled={creating || composer.items.length === 0}
                  >
                    {creating ? "Creating..." : "Create + Attach"}
                  </Button>
                )}
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
