import { useState, useCallback, useEffect, useRef } from "react";
import { parse as parseYAML, stringify as stringifyYAML } from "yaml";
import { useQueryClient } from "@tanstack/react-query";
import { Card } from "../Card";
import { Tabs } from "../Tabs";
import { Button } from "../Button";
import { DiffView, OVERLAY_RESOURCES } from "../OverlayEditor";
import { useToast } from "../Toast";
import ConflictDialog from "../ConflictDialog";
import {
  useOpenAPISchema,
  getPodOverridesSchema,
  getIngressSpecSchema,
  getMiteSpecSchema,
} from "../../api/openapiSchema";
import {
  updateController,
  ControllerConflictError,
  previewControllerOverlay,
} from "../../api/client";
import type {
  OverlayResourceKind,
  OverlayBaseline,
  OverlayWarning,
  PreviewResponse,
  PodOverrides,
  ResourceOverlay,
  IngressSpec,
  MiteSpec,
} from "../../types";
import ControllerSpecForm from "./ControllerSpecForm";
import YamlTierEditor from "./YamlTierEditor";
import styles from "./SpecEditorCard.module.css";

const SPEC_TABS = [
  { id: "form", label: <>Form</> },
  { id: "podOverrides", label: <>Pod overrides</> },
  { id: "resourceOverlay", label: <>Resource overlay</> },
  { id: "ingressSpec", label: <>Ingress</> },
  { id: "miteSpec", label: <>Mite sidecar</> },
];

const OVERLAY_TABS = OVERLAY_RESOURCES.map((r) => ({
  id: r.key,
  label: <>{r.label}</>,
}));

interface SpecEditorCardProps {
  cluster: string;
  ns: string;
  name: string;
  initialOverlay?: ResourceOverlay;
  initialPodOverrides?: PodOverrides;
  initialIngressSpec?: IngressSpec;
  initialMiteSpec?: MiteSpec;
  canUpdate?: boolean;
}

export default function SpecEditorCard({
  cluster,
  ns,
  name,
  initialOverlay,
  initialPodOverrides,
  initialIngressSpec,
  initialMiteSpec,
  canUpdate,
}: SpecEditorCardProps) {
  const { toast } = useToast();
  const qc = useQueryClient();
  const { data: openapiRoot } = useOpenAPISchema();

  // ── Tier 1: spec form state ──
  const [specValue, setSpecValue] = useState<Record<string, unknown>>({});

  // ── Tier 2: podOverrides text ──
  const [podOverridesText, setPodOverridesText] = useState(() =>
    initialPodOverrides ? stringifyYAML(initialPodOverrides) : "",
  );

  // ── Tier 2: ingressSpec / miteSpec YAML text ──
  // Both fields mix free-form key/value maps (ingressSpec.annotations,
  // miteSpec.resources.requests/limits) with the RJSF-generated form, which
  // rendered them unusably (issue #429). YAML is the honest editor here,
  // same as podOverrides.
  const [ingressSpecText, setIngressSpecText] = useState(() =>
    initialIngressSpec ? stringifyYAML(initialIngressSpec) : "",
  );
  const [miteSpecText, setMiteSpecText] = useState(() =>
    initialMiteSpec ? stringifyYAML(initialMiteSpec) : "",
  );

  // ── Tier 3: resource overlay texts ──
  const [statefulSet, setStatefulSet] = useState(() => initialOverlay?.statefulSet ?? "");
  const [service, setService] = useState(() => initialOverlay?.service ?? "");
  const [ingress, setIngress] = useState(() => initialOverlay?.ingress ?? "");

  const overlayValues: Record<OverlayResourceKind, string> = { statefulSet, service, ingress };
  const overlaySetters: Record<OverlayResourceKind, (v: string) => void> = {
    statefulSet: setStatefulSet,
    service: setService,
    ingress: setIngress,
  };

  // ── Tab state ──
  const [activeTab, setActiveTab] = useState("form");
  const [overlaySubTab, setOverlaySubTab] = useState("statefulSet");

  // ── Preview state ──
  const [baseline] = useState<OverlayBaseline>("live");
  const [preview, setPreview] = useState<PreviewResponse | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [forbidden, setForbidden] = useState(false);

  // ── Conflict dialog ──
  const [conflicts, setConflicts] = useState<import("../../api/client").ConflictInfo[]>([]);
  const [showConflict, setShowConflict] = useState(false);
  // Stash the patch being attempted so Override can retry
  const pendingPatch = useRef<Record<string, unknown> | null>(null);

  // ── PodOverrides schema for Tier 2 validation ──
  const podOverridesSchema = openapiRoot ? getPodOverridesSchema(openapiRoot) : undefined;
  const ingressSpecSchema = openapiRoot ? getIngressSpecSchema(openapiRoot) : undefined;
  const miteSpecSchema = openapiRoot ? getMiteSpecSchema(openapiRoot) : undefined;

  // ── Build preview body from all tiers ──
  const buildBody = useCallback(() => {
    const resourceOverlay: ResourceOverlay = {};
    if (statefulSet.trim()) resourceOverlay.statefulSet = statefulSet;
    if (service.trim()) resourceOverlay.service = service;
    if (ingress.trim()) resourceOverlay.ingress = ingress;

    let po: PodOverrides | undefined;
    if (podOverridesText.trim()) {
      try {
        po = parseYAML(podOverridesText) as PodOverrides;
      } catch {
        // parse error — caller should gate on validity
        return null;
      }
    }

    return { podOverrides: po, resourceOverlay, baseline };
  }, [statefulSet, service, ingress, podOverridesText, baseline]);

  const hasOverlayContent =
    !!statefulSet.trim() || !!service.trim() || !!ingress.trim() || !!podOverridesText.trim();

  // ── Debounced live preview (Tier 2/3 only) ──
  const previewTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const debouncedPreview = useCallback(
    (_text: string) => {
      if (previewTimer.current) clearTimeout(previewTimer.current);
      previewTimer.current = setTimeout(async () => {
        // Re-read the latest state from the callbacks
        // We always build from current state (setters are stable)
        if (!hasOverlayContent) {
          setPreview(null);
          setPreviewError(null);
          return;
        }
        const body = buildBody();
        if (!body) return;
        setPreviewing(true);
        setPreviewError(null);
        try {
          const res = await previewControllerOverlay(cluster, ns, name, body);
          setPreview(res);
          setForbidden(false);
        } catch (e) {
          const msg = e instanceof Error ? e.message : "preview failed";
          if (msg.startsWith("403")) {
            setForbidden(true);
          } else {
            setPreviewError(msg);
          }
        } finally {
          setPreviewing(false);
        }
      }, 600);
    },
    [cluster, ns, name, hasOverlayContent, buildBody],
  );

  // Trigger preview when overlay content changes
  useEffect(() => {
    if (!hasOverlayContent) {
      setPreview(null);
      setPreviewError(null);
      return;
    }
    debouncedPreview("");
    return () => {
      if (previewTimer.current) clearTimeout(previewTimer.current);
    };
  }, [statefulSet, service, ingress, podOverridesText]);

  // ── Save handler ──
  const doSave = useCallback(
    async (force: boolean) => {
      // Parse every YAML tier BEFORE touching the API. A tier that doesn't
      // parse must abort the save outright: silently omitting it from the
      // patch leaves the server value untouched while the user still gets a
      // "Spec saved" toast, i.e. their edit is discarded without any signal.
      const parsedTiers: Record<string, unknown> = {};
      for (const tier of [
        { key: "podOverrides", text: podOverridesText, label: "Pod overrides" },
        { key: "ingressSpec", text: ingressSpecText, label: "Ingress" },
        { key: "miteSpec", text: miteSpecText, label: "Mite" },
      ]) {
        if (!tier.text.trim()) continue;
        try {
          parsedTiers[tier.key] = parseYAML(tier.text);
        } catch (e) {
          toast(
            `${tier.label} YAML is invalid: ${e instanceof Error ? e.message : "parse error"}`,
          );
          return;
        }
      }

      setSaving(true);
      try {
        // Build patch from Tier 1 spec diff
        const patch: Record<string, unknown> = { spec: {} };
        const specPatch = patch.spec as Record<string, unknown>;

        // Diff spec: include only changed top-level keys
        for (const key of Object.keys(specValue)) {
          specPatch[key] = specValue[key];
        }

        // Tier 2: podOverrides
        if (podOverridesText.trim()) {
          specPatch.podOverrides = parsedTiers.podOverrides;
        } else if (initialPodOverrides) {
          // Clear if previously set and now empty
          specPatch.podOverrides = null;
        }

        // Tier 2: ingressSpec
        if (ingressSpecText.trim()) {
          specPatch.ingressSpec = parsedTiers.ingressSpec;
        } else if (initialIngressSpec) {
          specPatch.ingressSpec = null;
        }

        // Tier 2: miteSpec
        if (miteSpecText.trim()) {
          specPatch.miteSpec = parsedTiers.miteSpec;
        } else if (initialMiteSpec) {
          specPatch.miteSpec = null;
        }

        // Tier 3: resourceOverlay
        const roPatch: Record<string, string> = {};
        let hasRO = false;
        for (const key of ["statefulSet", "service", "ingress"] as const) {
          const v =
            key === "statefulSet" ? statefulSet : key === "service" ? service : ingress;
          if (v.trim()) {
            roPatch[key] = v;
            hasRO = true;
          }
        }
        if (hasRO || initialOverlay) {
          specPatch.resourceOverlay = hasRO ? roPatch : null;
        }

        pendingPatch.current = patch;
        await updateController(cluster, name, ns, patch, { force });
        setShowConflict(false);
        setConflicts([]);
        toast("Spec saved");
        qc.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      } catch (e) {
        if (e instanceof ControllerConflictError) {
          setConflicts(e.conflicts);
          setShowConflict(true);
        } else {
          toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
        }
      }
      setSaving(false);
    },
    [
      cluster, name, ns, specValue,
      podOverridesText, initialPodOverrides,
      ingressSpecText, initialIngressSpec,
      miteSpecText, initialMiteSpec,
      statefulSet, service, ingress, initialOverlay,
      qc, toast,
    ],
  );

  const handleSave = () => doSave(false);
  const handleOverride = () => doSave(true);
  const handleReload = () => {
    setShowConflict(false);
    setConflicts([]);
    qc.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
  };

  if (forbidden) return null;

  const warnings: OverlayWarning[] = preview?.warnings ?? [];

  return (
    <>
      <Card
        title="Spec Editor"
        headerRight={<span className={styles.cardNote}>Saving rolls the pod</span>}
      >
        <Tabs tabs={SPEC_TABS} activeTab={activeTab} onSelect={setActiveTab} />

        <div className={styles.tabContent}>
          {/* Tier 1: Generated form */}
          {activeTab === "form" && (
            <ControllerSpecForm
              value={specValue}
              onChange={(v) => setSpecValue(v)}
            />
          )}

          {/* Tier 2: Pod overrides YAML editor */}
          {activeTab === "podOverrides" && (
            <div className={styles.yamlTier}>
              <div className={styles.tierLabel}>Pod overrides (YAML)</div>
              <YamlTierEditor
                value={podOverridesText}
                onChange={setPodOverridesText}
                jsonSchema={podOverridesSchema}
                onDebouncedChange={debouncedPreview}
              />
              {podOverridesSchema && (
                <div className={styles.schemaNote}>
                  Validated against PodOverrides schema
                </div>
              )}
            </div>
          )}

          {/* Tier 2: ingressSpec YAML editor — annotations is a free-form
              key/value map the generated form rendered unusably (#429) */}
          {activeTab === "ingressSpec" && (
            <div className={styles.yamlTier}>
              <div className={styles.tierLabel}>Ingress (YAML)</div>
              <YamlTierEditor
                value={ingressSpecText}
                onChange={setIngressSpecText}
                jsonSchema={ingressSpecSchema}
              />
              {ingressSpecSchema && (
                <div className={styles.schemaNote}>
                  Validated against IngressSpec schema
                </div>
              )}
            </div>
          )}

          {/* Tier 2: miteSpec YAML editor — resources.requests/limits are
              free-form maps the generated form rendered unusably (#429) */}
          {activeTab === "miteSpec" && (
            <div className={styles.yamlTier}>
              <div className={styles.tierLabel}>Mite sidecar (YAML)</div>
              <YamlTierEditor
                value={miteSpecText}
                onChange={setMiteSpecText}
                jsonSchema={miteSpecSchema}
              />
              {miteSpecSchema && (
                <div className={styles.schemaNote}>
                  Validated against MiteSpec schema
                </div>
              )}
            </div>
          )}

          {/* Tier 3: Resource overlay YAML editors */}
          {activeTab === "resourceOverlay" && (
            <div className={styles.yamlTier}>
              <Tabs
                tabs={OVERLAY_TABS}
                activeTab={overlaySubTab}
                onSelect={setOverlaySubTab}
              />
              <div style={{ marginTop: 12 }}>
                <YamlTierEditor
                  value={overlayValues[overlaySubTab as OverlayResourceKind]}
                  onChange={(v: string) =>
                    overlaySetters[overlaySubTab as OverlayResourceKind](v)
                  }
                  onDebouncedChange={debouncedPreview}
                />
              </div>
            </div>
          )}
        </div>

        {/* Preview section (overlays only) */}
        {hasOverlayContent && (activeTab === "podOverrides" || activeTab === "resourceOverlay") && (
          <div className={styles.previewSection}>
            <div className={styles.previewHeader}>
              <span className={styles.previewLabel}>Live merge preview</span>
              {previewing && <span className={styles.muted}>⟳ previewing…</span>}
            </div>
            {previewError && (
              <div className={styles.errorBanner}>{previewError}</div>
            )}
            {!preview && !previewError && (
              <div className={styles.muted}>Edit an overlay to see the merged diff.</div>
            )}
            {preview &&
              OVERLAY_RESOURCES.map(({ key, label }) =>
                preview.diff[key] !== undefined ? (
                  <div key={key} style={{ marginBottom: 12 }}>
                    <div className={styles.previewResourceLabel}>{label}</div>
                    <DiffView diff={preview.diff[key] ?? ""} />
                  </div>
                ) : null,
              )}
            {warnings.length > 0 && (
              <div className={styles.warningList}>
                {warnings.map((w, i) => (
                  <div key={i} className={styles.warningItem}>
                    ⚠ {w.resource}: {w.message}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}

        {/* Save button */}
        {canUpdate && (
          <div style={{ marginTop: 12 }}>
            <Button
              size="sm"
              variant="primary"
              disabled={saving}
              onClick={handleSave}
            >
              {saving ? "Saving…" : "Save spec"}
            </Button>
          </div>
        )}
      </Card>

      <ConflictDialog
        conflicts={conflicts}
        open={showConflict}
        onReload={handleReload}
        onOverride={handleOverride}
      />
    </>
  );
}
