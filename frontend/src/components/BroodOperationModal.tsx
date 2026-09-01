import { useEffect, useMemo, useRef, useState, useCallback } from "react";
import { useQuery } from "@tanstack/react-query";
import { previewBroodOperation, createBroodOperation, getProvisioningConfig } from "../api/client";
import { Button } from "./Button";
import VersionPicker from "./VersionPicker";
import { broodTargetShape } from "../lib/broodTargets";
import { parseClearableInt, clampClearableInt } from "../lib/numericInput";
import { useCatalogItems } from "../hooks/useCatalog";
import type { BroodVerb, BroodOrder, BroodFailurePolicy, BroodPreviewTarget, BroodRun, BroodAction } from "../types";
import styles from "./BroodOperationModal.module.css";

interface BroodOperationModalProps {
  targets: string[];
  onClose: () => void;
  onSubmitted: (result: BroodRun) => void;
  /** Renders only the inner form/preview/actions, without the overlay+dialog chrome
   *  or its own title — for embedding inside a parent-supplied dialog. */
  embedded?: boolean;
}

export function BroodOperationModal({ targets, onClose, onSubmitted, embedded = false }: BroodOperationModalProps) {
  const [verb, setVerb] = useState<BroodVerb>("reconcile");
  const [maxParallel, setMaxParallel] = useState<number | "">(1);
  const [order, setOrder] = useState<BroodOrder>("rolloutWave");
  const [failurePolicy, setFailurePolicy] = useState<BroodFailurePolicy>("FailTidy");
  const [preview, setPreview] = useState<BroodPreviewTarget[] | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const previewRequestRef = useRef(0);
  const mountedRef = useRef(true);

  // ---- Groovy state ----
  const [groovyMode, setGroovyMode] = useState<"script" | "itemRef">("script");
  const [script, setScript] = useState("");
  const [selectedItemKey, setSelectedItemKey] = useState(""); // "namespace/name"
  const [groovyVars, setGroovyVars] = useState<Record<string, string>>({}); // itemRef variable overrides

  // ---- Upgrade state ----
  const [upgradeMode, setUpgradeMode] = useState<"release" | "setVersion">("release");
  const [upgradeLts, setUpgradeLts] = useState(false); // "Latest LTS" checkbox
  const [upgradePicked, setUpgradePicked] = useState(""); // VersionPicker's exact-version value
  const [upgradeLine, setUpgradeLine] = useState(""); // line-text-input override

  // Distinct target clusters
  const targetClusters = useMemo(
    () => Array.from(new Set(targets.map(t => t.split("/")[0]).filter(Boolean))),
    [targets],
  );
  const singleCluster = targetClusters.length === 1 ? targetClusters[0] : null;

  // The server filters by type; keep the groovy+valid check as a client-side invariant
  // because executeGroovy is a privileged surface.
  const { data: catalogData, isLoading: itemsLoading, error: itemsError } =
    useCatalogItems(verb === "executeGroovy" && groovyMode === "itemRef" ? singleCluster : null, { type: "groovy" });
  const groovyItems = useMemo(
    () => (catalogData?.items ?? []).filter(i => i.type === "groovy" && i.valid),
    [catalogData],
  );
  const selectedItem = useMemo(
    () => groovyItems.find(i => `${i.namespace}/${i.name}` === selectedItemKey),
    [groovyItems, selectedItemKey],
  );

  // Declared variables, DEDUPED by name (first-wins)
  const declaredVars = useMemo(() => {
    const seen = new Set<string>();
    return (selectedItem?.variables ?? []).filter(v => (seen.has(v.name) ? false : (seen.add(v.name), true)));
  }, [selectedItem]);

  // Version catalog for the upgrade verb's "Move to version" picker — sourced from
  // targetClusters[0] (a representative cluster; see the multi-cluster note rendered below).
  const { data: upgradeProvisioningConfig } = useQuery({
    queryKey: ["provisioning-config", targetClusters[0]],
    queryFn: () => getProvisioningConfig(targetClusters[0]),
    enabled: targetClusters.length >= 1 && upgradeMode === "setVersion",
  });
  const upgradeVersions = upgradeProvisioningConfig?.versions ?? [];

  // ---- Race-safe preview invalidation ----
  const invalidatePreview = useCallback(() => {
    previewRequestRef.current++;
    setPreview(null);
    setError("");
    setPreviewLoading(false);
  }, []);

  // The embedded wizard keeps the selection editable while this preview is
  // showing (BroodOperations.tsx), so a changed `targets` must invalidate
  // whatever preview/error is currently displayed.
  useEffect(() => {
    invalidatePreview();
  }, [targets, invalidatePreview]);

  // Reset itemRef selection when the cluster changes
  useEffect(() => {
    setSelectedItemKey("");
    setGroovyVars({});
    invalidatePreview();
  }, [singleCluster, invalidatePreview]);

  // Drop stale selection if the catalog list refreshes and item disappears
  useEffect(() => {
    if (selectedItemKey && !itemsLoading && !selectedItem) {
      setSelectedItemKey("");
      setGroovyVars({});
      invalidatePreview();
    }
  }, [selectedItemKey, selectedItem, itemsLoading, invalidatePreview]);

  // The embedded wizard (BroodOperations.tsx) unmounts this modal as soon as
  // the last target is deselected, which can happen while a preview/create
  // request is still in-flight — guard state updates against that.
  useEffect(() => {
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    if (embedded) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [embedded, onClose]);

  // ---- Action builder (used by both preview & create) ----
  function buildAction(): BroodAction {
    if (verb === "upgrade") {
      if (upgradeMode === "release") return { verb, upgrade: {} };
      const targetVersion = upgradeLts ? "lts" : (upgradeLine.trim() || upgradePicked);
      return { verb, upgrade: { targetVersion } };
    }
    if (verb !== "executeGroovy") return { verb };
    if (groovyMode === "script") return { verb, groovy: { script } };
    // itemRef mode. The Preview/Create buttons are disabled until an item is selected,
    // so this should not be reachable empty; guard defensively rather than assert, so a
    // stale/cleared selection surfaces as a user-facing error (callers catch and setError).
    if (!selectedItem) throw new Error("Select a catalog item.");
    // Materialize declared defaults client-side (backend ResolveVars does NOT)
    const vars: Record<string, string> = {};
    for (const dv of declaredVars) {
      const raw = groovyVars[dv.name];
      const val = raw !== undefined && raw.trim() !== "" ? raw : (dv.default ?? "");
      if (val !== "") vars[dv.name] = val;
    }
    return {
      verb,
      groovy: {
        itemRef: {
          name: selectedItem.name,
          namespace: selectedItem.namespace,
          ...(Object.keys(vars).length ? { variables: vars } : {}),
        },
      },
    };
  }

  const runBroodPreview = async () => {
    setPreviewLoading(true);
    setError("");
    setPreview(null);
    const requestId = ++previewRequestRef.current;
    const shape = broodTargetShape(targets);
    try {
      const result = await previewBroodOperation({
        namespace: shape.namespace,
        spec: {
          action: buildAction(),
          targets: shape.names ? { names: shape.names } : { selector: {} },
          execution: { maxParallel: maxParallel !== "" && maxParallel > 1 ? maxParallel : undefined, order, failurePolicy },
        },
        clusters: shape.clusters,
      });
      if (previewRequestRef.current !== requestId || !mountedRef.current) return;
      setPreview(result.clusters[0]?.targets ?? null);
    } catch (e: any) {
      if (previewRequestRef.current !== requestId || !mountedRef.current) return;
      setError(e.message ?? "Preview failed");
    } finally {
      if (previewRequestRef.current === requestId && mountedRef.current) setPreviewLoading(false);
    }
  };

  const runBroodCreate = async () => {
    setCreating(true);
    setError("");
    const shape = broodTargetShape(targets);
    try {
      const result = await createBroodOperation({
        namespace: shape.namespace,
        spec: {
          action: buildAction(),
          targets: shape.names ? { names: shape.names } : { selector: {} },
          execution: {
            maxParallel: maxParallel !== "" && maxParallel > 1 ? maxParallel : undefined,
            order,
            failurePolicy,
          },
        },
        clusters: shape.clusters,
      });
      if (!mountedRef.current) return;
      setCreating(false);
      onSubmitted(result);
    } catch (e: any) {
      if (!mountedRef.current) return;
      setCreating(false);
      setError(e.message ?? "Create failed");
    }
  };

  // ---- Derived disabled states ----
  const requiredVarMissing = groovyMode === "itemRef" && !!selectedItem &&
    declaredVars.some(v => v.required && !(groovyVars[v.name]?.trim()) && !v.default);
  const groovyIncomplete = verb === "executeGroovy" &&
    (groovyMode === "script" ? script.trim() === "" : (!selectedItem || requiredVarMissing));

  // Build disable-reason hints
  let disableHint = "";
  if (verb === "executeGroovy" && groovyIncomplete) {
    if (groovyMode === "script") {
      disableHint = "Enter a script";
    } else if (!selectedItem) {
      disableHint = "Select a catalog item";
    } else if (requiredVarMissing) {
      const missing = declaredVars
        .filter(v => v.required && !(groovyVars[v.name]?.trim()) && !v.default)
        .map(v => v.name);
      disableHint = `Fill required variables: ${missing.join(", ")}`;
    }
  }

  // ---- Verb change handler ----
  const handleVerbChange = (newVerb: BroodVerb) => {
    setVerb(newVerb);
    setGroovyMode("script");
    setScript("");
    setSelectedItemKey("");
    setGroovyVars({});
    setUpgradeMode("release");
    setUpgradeLts(false);
    setUpgradePicked("");
    setUpgradeLine("");
    invalidatePreview();
  };

  const content = (
    <>
      {!embedded && <div id="brood-operation-modal-title" className={styles.dialogTitle}>Run Brood Operation</div>}

      <div className={styles.broodForm}>
        <label>Verb:
          <select value={verb} onChange={(e) => { handleVerbChange(e.target.value as BroodVerb); }}>
            <option value="restart">restart</option>
            <option value="reprovision">reprovision</option>
            <option value="reconcile">reconcile</option>
            <option value="stop">stop</option>
            <option value="start">start</option>
            <option value="executeGroovy">executeGroovy</option>
            <option value="upgrade">upgrade</option>
          </select>
        </label>
        <label>Max parallel:
          <input
            type="number"
            min={1}
            value={maxParallel}
            onChange={(e) => { setMaxParallel(parseClearableInt(e.target.value)); invalidatePreview(); }}
            onBlur={() => setMaxParallel((v) => clampClearableInt(v, 1))}
          />
        </label>
        <label>Order:
          <select value={order} onChange={(e) => { setOrder(e.target.value as BroodOrder); invalidatePreview(); }}>
            <option value="rolloutWave">rolloutWave</option>
            <option value="name">name</option>
          </select>
        </label>
        <label>Failure policy:
          <select value={failurePolicy} onChange={(e) => { setFailurePolicy(e.target.value as BroodFailurePolicy); invalidatePreview(); }}>
            <option value="FailFast">FailFast</option>
            <option value="FailTidy">FailTidy</option>
            <option value="FailAtEnd">FailAtEnd</option>
          </select>
        </label>
      </div>

      {/* ---- Groovy sub-form ---- */}
      {verb === "executeGroovy" && (
        <div className={styles.groovyBlock} role="region" aria-label="Groovy script configuration">
          {/* RCE warning callout */}
          <div className={styles.groovyWarn} role="note">
            <span aria-hidden="true">⚠</span>
            <span>Runs arbitrary Groovy on every targeted controller's Jenkins. Requires controller-manage permission; operator-namespace runs require admin.</span>
          </div>

          {/* Mode toggle — segmented control */}
          <div className={styles.groovyModeToggle} role="group" aria-label="Groovy source">
            <button
              type="button"
              aria-pressed={groovyMode === "script"}
              onClick={() => {
                if (groovyMode !== "script") {
                  setGroovyMode("script");
                  setSelectedItemKey("");
                  setGroovyVars({});
                  invalidatePreview();
                }
              }}
            >
              Inline script
            </button>
            <button
              type="button"
              aria-pressed={groovyMode === "itemRef"}
              onClick={() => {
                if (groovyMode !== "itemRef") {
                  setGroovyMode("itemRef");
                  setSelectedItemKey("");
                  setGroovyVars({});
                  invalidatePreview();
                }
              }}
            >
              Catalog item
            </button>
          </div>

          {groovyMode === "script" ? (
            <label>
              Script
              <textarea
                className={styles.groovyScript}
                value={script}
                onChange={(e) => { setScript(e.target.value); invalidatePreview(); }}
                rows={10}
                placeholder={'println Jenkins.VERSION'}
              />
            </label>
          ) : (
            <div className={styles.groovyItemRef}>
              {singleCluster == null ? (
                <span className={styles.groovyMuted}>
                  Catalog-item source needs a single target cluster (selected: {targetClusters.length}). Use inline script, or narrow the target selection.
                </span>
              ) : itemsLoading ? (
                <span className={styles.groovyMuted}>Loading Groovy catalog items…</span>
              ) : itemsError ? (
                <span className={styles.errorText}>{(itemsError as any)?.message ?? "Failed to load catalog items."}</span>
              ) : groovyItems.length === 0 ? (
                <span className={styles.groovyMuted}>No Groovy catalog items on {singleCluster}.</span>
              ) : (
                <>
                  <label>
                    Catalog item
                    <select
                      value={selectedItemKey}
                      onChange={(e) => {
                        setSelectedItemKey(e.target.value);
                        setGroovyVars({});
                        invalidatePreview();
                      }}
                    >
                      <option value="">— select —</option>
                      {groovyItems.map(i => (
                        <option key={`${i.namespace}/${i.name}`} value={`${i.namespace}/${i.name}`}>
                          {i.displayName ?? i.name} ({i.namespace})
                        </option>
                      ))}
                    </select>
                  </label>

                  {/* Variables form */}
                  {declaredVars.length > 0 && selectedItemKey && (
                    <div className={styles.groovyVars}>
                      <span className={styles.groovyVarsHeading}>Variables</span>
                      {declaredVars.map((v, idx) => {
                        const t = v.type || "string";
                        const set = (name: string, val: string) => {
                          setGroovyVars(p => ({ ...p, [name]: val }));
                          invalidatePreview();
                        };
                        const currentVal = groovyVars[v.name];

                        let control: React.ReactNode;
                        if (t === "boolean") {
                          control = (
                            <input
                              type="checkbox"
                              id={`groovy-var-${v.name}`}
                              checked={(currentVal ?? v.default ?? "false") === "true"}
                              onChange={(e) => set(v.name, e.target.checked ? "true" : "false")}
                            />
                          );
                        } else if (v.allowedValues?.length && (t === "string" || t === "number")) {
                          control = (
                            <select
                              id={`groovy-var-${v.name}`}
                              value={currentVal ?? ""}
                              onChange={(e) => set(v.name, e.target.value)}
                            >
                              <option value="">Default ({v.default ?? "—"})</option>
                              {v.allowedValues.map(av => (
                                <option key={av} value={av}>{av}</option>
                              ))}
                            </select>
                          );
                        } else if (t === "number") {
                          control = (
                            <input
                              type="number"
                              id={`groovy-var-${v.name}`}
                              value={currentVal ?? ""}
                              placeholder={v.default ?? ""}
                              onChange={(e) => set(v.name, e.target.value)}
                            />
                          );
                        } else {
                          control = (
                            <input
                              type="text"
                              id={`groovy-var-${v.name}`}
                              value={currentVal ?? ""}
                              placeholder={v.default ?? ""}
                              onChange={(e) => set(v.name, e.target.value)}
                            />
                          );
                        }

                        return (
                          <div key={`${v.name}-${idx}`} className={t === "boolean" ? styles.groovyVarRowInline : styles.groovyVarRow}>
                            <label htmlFor={`groovy-var-${v.name}`} className={styles.groovyVarLabel}>
                              {v.name}
                              {v.required && <span className={styles.groovyReq}>*</span>}
                            </label>
                            {control}
                            {v.description && <span className={styles.groovyMuted}>{v.description}</span>}
                            {v.default !== undefined && v.default !== "" && (
                              <span className={styles.groovyMuted}>Default: {v.default}</span>
                            )}
                          </div>
                        );
                      })}
                    </div>
                  )}
                </>
              )}
            </div>
          )}
        </div>
      )}

      {/* ---- Upgrade sub-form ---- */}
      {verb === "upgrade" && (
        <div className={styles.upgradeBlock} role="region" aria-label="Upgrade configuration">
          <div className={styles.upgradeModeToggle} role="group" aria-label="Upgrade mode">
            <button
              type="button"
              aria-pressed={upgradeMode === "release"}
              onClick={() => {
                if (upgradeMode !== "release") {
                  setUpgradeMode("release");
                  invalidatePreview();
                }
              }}
            >
              Release held upgrade
            </button>
            <button
              type="button"
              aria-pressed={upgradeMode === "setVersion"}
              onClick={() => {
                if (upgradeMode !== "setVersion") {
                  setUpgradeMode("setVersion");
                  invalidatePreview();
                }
              }}
            >
              Move to version
            </button>
          </div>

          {upgradeMode === "release" ? (
            <span className={styles.groovyMuted}>Releases each target's currently held promoted upgrade.</span>
          ) : (
            <>
              <label>
                <input
                  type="checkbox"
                  checked={upgradeLts}
                  onChange={(e) => {
                    const checked = e.target.checked;
                    setUpgradeLts(checked);
                    if (checked) {
                      setUpgradePicked("");
                      setUpgradeLine("");
                    }
                    invalidatePreview();
                  }}
                />{" "}
                Latest LTS
              </label>

              <VersionPicker
                versions={upgradeVersions}
                value={upgradePicked}
                onChange={(v) => { setUpgradePicked(v); invalidatePreview(); }}
                disabled={upgradeLts}
              />

              <label>
                or enter a line (e.g. 2.555)
                <input
                  type="text"
                  value={upgradeLine}
                  disabled={upgradeLts}
                  onChange={(e) => { setUpgradeLine(e.target.value); invalidatePreview(); }}
                />
              </label>
              <span className={styles.groovyMuted}>Leave blank to match the baseline pin.</span>

              {targetClusters.length > 1 && (
                <span className={styles.groovyMuted}>
                  Versions shown are from {targetClusters[0]}'s catalog; the value you pick or enter is resolved
                  against each target's own profile when the operation runs.
                </span>
              )}
            </>
          )}
        </div>
      )}

      <div className={styles.broodPreview}>
        <Button onClick={runBroodPreview} disabled={previewLoading || creating || groovyIncomplete}>
          {previewLoading ? "Previewing…" : "Preview"}
        </Button>

        {preview && (
          <table className={styles.broodTable}>
            <thead>
              <tr><th>Namespace</th><th>Name</th><th>Wave</th><th>Applicable</th><th>Reason</th></tr>
            </thead>
            <tbody>
              {preview.map((t, i) => (
                <tr key={i}>
                  <td>{t.namespace}</td>
                  <td>{t.name}</td>
                  <td>{t.wave}</td>
                  <td>{t.applicable ? "yes" : "no"}</td>
                  <td>{t.reason ?? "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {error && <p className={styles.errorText}>{error}</p>}

      {groovyIncomplete && disableHint && (
        <p className={styles.groovyMuted} style={{ marginBottom: 8 }}>{disableHint}</p>
      )}

      <div className={styles.dialogActions}>
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" onClick={runBroodCreate} disabled={creating || previewLoading || groovyIncomplete}>
          {creating ? "Creating…" : "Create & Run"}
        </Button>
      </div>
    </>
  );

  if (embedded) {
    return content;
  }

  return (
    <div className={styles.overlay} onClick={onClose}>
      <div
        className={styles.dialog}
        role="dialog"
        aria-modal="true"
        aria-labelledby="brood-operation-modal-title"
        onClick={(e) => e.stopPropagation()}
      >
        {content}
      </div>
    </div>
  );
}
