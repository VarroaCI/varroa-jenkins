import { useState, useCallback, useEffect, useMemo, useRef } from "react";
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
  ControllerSpec,
} from "../../types";
import type { RJSFValidationError } from "@rjsf/utils";
import ControllerSpecForm from "./ControllerSpecForm";
import YamlTierEditor from "./YamlTierEditor";
import { EXCLUDED_FROM_TIER1 } from "./excludedFields";
import { unappliedRemovalNotice } from "../../lib/unappliedRemovals";
import { KeyEditContext } from "../form/KeyEditContext";
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

const OVERLAY_KEYS = ["statefulSet", "service", "ingress"] as const;

// Every independently-editable draft carries its own version. A tier's draft is
// dirty iff its version is > 0. Versions are captured when a save starts and,
// on success, only tiers whose version is UNCHANGED since then are rebased —
// an edit made while the save was in flight keeps its tier out of the rebase.
const DRAFT_TIERS = [
  "form",
  "podOverrides",
  "ingressSpec",
  "miteSpec",
  "statefulSet",
  "service",
  "ingress",
] as const;
type DraftTier = (typeof DRAFT_TIERS)[number];

const ZERO_VERSIONS: Record<DraftTier, number> = {
  form: 0,
  podOverrides: 0,
  ingressSpec: 0,
  miteSpec: 0,
  statefulSet: 0,
  service: 0,
  ingress: 0,
};

// ── Recursive diff ──────────────────────────────────────────────────────────
// The save patch is a REAL recursive diff of every tier's draft against the
// immutable baseline: changed values are emitted and each removed key (absent
// from the draft at any depth) is emitted as an explicit JSON null. `undefined`
// would vanish through JSON.stringify, so removals must be null.
const NO_CHANGE = Symbol("no-change");

function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a === null || b === null || typeof a !== typeof b) return false;
  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
    return a.every((v, i) => deepEqual(v, b[i]));
  }
  if (typeof a === "object" && typeof b === "object") {
    const aKeys = Object.keys(a as Record<string, unknown>);
    const bKeys = Object.keys(b as Record<string, unknown>);
    if (aKeys.length !== bKeys.length) return false;
    return aKeys.every((k) =>
      deepEqual((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k]),
    );
  }
  return false;
}

function diffValues(baseline: unknown, draft: unknown): unknown {
  // A key removed from the draft is a removal request (explicit null). A draft
  // value of `undefined` is treated as absent — stringify would drop it anyway.
  if (draft === undefined) {
    return baseline === undefined ? NO_CHANGE : null;
  }
  if (baseline === undefined) {
    // A key the draft introduced that the baseline never had.
    return draft;
  }
  if (deepEqual(baseline, draft)) return NO_CHANGE;
  if (
    typeof baseline === "object" &&
    baseline !== null &&
    !Array.isArray(baseline) &&
    typeof draft === "object" &&
    draft !== null &&
    !Array.isArray(draft)
  ) {
    const out: Record<string, unknown> = {};
    const base = baseline as Record<string, unknown>;
    const dr = draft as Record<string, unknown>;
    for (const key of Object.keys(base)) {
      const sub = diffValues(base[key], dr[key]);
      if (sub !== NO_CHANGE) out[key] = sub;
    }
    for (const key of Object.keys(dr)) {
      if (!(key in base)) {
        const sub = diffValues(undefined, dr[key]);
        if (sub !== NO_CHANGE) out[key] = sub;
      }
    }
    return Object.keys(out).length > 0 ? out : NO_CHANGE;
  }
  // A changed scalar or list: emit the new value as-is.
  return draft;
}

// ── Save validation gate ─────────────────────────────────────────────────────
// The curated tier runs live validation now, and Save is gated by CONTAINMENT,
// not equality: Save is blocked only when an invalid field path is at or below
// a path present in the curated patch (the fields actually being sent). A
// pre-existing invalid value the user has NOT edited sits outside the patch and
// does not block — someone editing only a YAML tier or only className can still
// save past a stale invalid `resources.limits.cpu`. The one exception is an
// error whose path does NOT resolve to a real form value (a mangled map key, or
// a compilation error with no path): that blocks unconditionally, fail closed
// (§2 — accepted limitation, see design.md §9).
//
// Fail closed, via one rule: an error whose path does NOT resolve to a real
// value in the form data blocks unconditionally. This covers (a) map keys
// containing `.`/`/`/`~`, which RJSF mangles (`toErrorSchema` splits on `.`,
// ajv escapes `/`→`~1`, so `nvidia.com/gpu` reports at
// `.resources.limits.nvidia.com~1gpu`, which never matches the real key), and
// (b) a property-less error (an ajv schema-compilation failure carries only
// `stack`) — without the rule a schema that failed to compile would make the
// whole form read as valid. Array indices are ordinary path segments.

/** Split an RJSF error `property` (`.resources.limits.cpu`) into path
 * segments. A missing or empty property (root or compilation errors) yields
 * null — such an error has no usable path. */
function errorPathSegments(property: string | undefined): string[] | null {
  if (!property) return null;
  const segments = property.replace(/^\./, "").split(".").filter(Boolean);
  return segments.length > 0 ? segments : null;
}

/** True when `property` walks to a value that actually exists in the form data.
 * Each segment must land on an existing non-undefined value, and the walked
 * node must stay an object (or an array for a numeric segment). A mangled map
 * key path fails here by construction — no un-mangling, no parallel validator. */
function errorPathResolves(property: string | undefined, formData: unknown): boolean {
  const segments = errorPathSegments(property);
  if (!segments) return false;
  let node: unknown = formData;
  for (const segment of segments) {
    if (node === null || typeof node !== "object") return false;
    const exists = Array.isArray(node)
      ? /^\d+$/.test(segment) && Number(segment) < node.length
      : Object.prototype.hasOwnProperty.call(node, segment);
    if (!exists) return false;
    node = (node as Record<string, unknown>)[segment];
    if (node === undefined) return false;
  }
  return true;
}

/** All root-to-leaf paths present in a diff patch. A leaf is any non-plain-object
 * value — `diffValues` emits arrays and scalars wholesale, so a patch only ever
 * descends through plain objects and every leaf is a whole-replacement path. */
function patchLeafPaths(patch: unknown, prefix: string[] = []): string[][] {
  if (patch && typeof patch === "object" && !Array.isArray(patch)) {
    const out: string[][] = [];
    for (const [key, value] of Object.entries(patch)) {
      out.push(...patchLeafPaths(value, [...prefix, key]));
    }
    return out;
  }
  return [prefix];
}

function isPrefixOf(prefix: string[], path: string[]): boolean {
  if (prefix.length > path.length) return false;
  return prefix.every((segment, i) => segment === path[i]);
}

/** The invalid field paths that block this save, as display strings. Empty when
 * the save may proceed. Two routes into the list:
 *  - an error whose path fails to resolve to a real form value blocks
 *    unconditionally (fail closed — mangled map keys, compilation errors);
 *  - a resolvable error blocks only when its path is at or below a path present
 *    in the curated patch (the fields being sent).
 * `curatedPatch` may be the NO_CHANGE symbol (the tier contributes nothing). */
function blockingErrorPaths(
  errors: RJSFValidationError[],
  formData: Record<string, unknown>,
  curatedPatch: unknown,
): string[] {
  const leaves = curatedPatch === NO_CHANGE ? [] : patchLeafPaths(curatedPatch);
  const blocked: string[] = [];
  for (const error of errors) {
    const display = (error.property ?? "").replace(/^\./, "");
    if (!errorPathResolves(error.property, formData)) {
      blocked.push(display || "(form)");
      continue;
    }
    const segments = errorPathSegments(error.property);
    if (segments && leaves.some((leaf) => isPrefixOf(leaf, segments))) {
      blocked.push(display);
    }
  }
  return blocked;
}

// Client-side approximation of the server applying `patch` to `base` — used only
// to rebase the baseline when the PATCH response omits `spec` (it always carries
// it in production; this keeps the editor correct against an older BFF).
function applyPatchToObject(
  base: Record<string, unknown>,
  patch: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...base };
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) {
      delete out[k];
    } else if (
      typeof v === "object" &&
      !Array.isArray(v) &&
      typeof out[k] === "object" &&
      out[k] !== null &&
      !Array.isArray(out[k])
    ) {
      out[k] = applyPatchToObject(out[k] as Record<string, unknown>, v as Record<string, unknown>);
    } else {
      out[k] = v;
    }
  }
  return out;
}

// The curated tier's draft is the spec minus fields owned by dedicated cards or
// YAML tiers. It must NOT include them: diffing the full spec would let a field
// RJSF happens not to render (or an excluded one) fall out of the draft and be
// emitted as a spurious removal.
function stripExcluded(spec: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(spec)) {
    if (!EXCLUDED_FROM_TIER1.includes(k)) out[k] = v;
  }
  return out;
}

function yamlOf(value: unknown): string {
  return value == null ? "" : stringifyYAML(value);
}

// Rebase a YAML tier from a server value WITHOUT discarding the user's
// formatting. Re-stringifying the server's JSON silently normalizes the tier
// (drops comments, re-quotes, reorders keys) every time it converges after a
// save or refetch. When the current text already parses to the same value as
// the rebased value, keep the text verbatim; only replace it when the server's
// value actually differs. Skipping the rebase entirely would reintroduce the
// stale-draft revert bug — a tier left stale against an advanced baseline
// re-sends old values.
function rebasedYamlText(currentText: string, newValue: unknown): string {
  const newText = yamlOf(newValue);
  if (!currentText) return newText;
  try {
    if (deepEqual(parseYAML(currentText), parseYAML(newText))) return currentText;
  } catch {
    // The current text no longer parses — fall through to the rebased value.
  }
  return newText;
}

function tierDraftValue(tier: DraftTier, base: Record<string, unknown>): unknown {
  const ro = base.resourceOverlay as Record<string, unknown> | undefined;
  switch (tier) {
    case "form":
      return stripExcluded(base);
    case "podOverrides":
      return yamlOf(base.podOverrides);
    case "ingressSpec":
      return yamlOf(base.ingressSpec);
    case "miteSpec":
      return yamlOf(base.miteSpec);
    case "statefulSet":
      return (ro?.statefulSet as string | undefined) ?? "";
    case "service":
      return (ro?.service as string | undefined) ?? "";
    case "ingress":
      return (ro?.ingress as string | undefined) ?? "";
  }
}

// The RAW baseline value a tier's save diff compares against. Unlike
// tierDraftValue (which yields the editable draft — YAML text for the YAML
// tiers), this yields the value the draft was hydrated from, so the save diff
// re-emits exactly what the user changed relative to what they saw.
function tierBaseValue(tier: DraftTier, base: Record<string, unknown>): unknown {
  const ro = base.resourceOverlay as Record<string, unknown> | undefined;
  switch (tier) {
    case "form":
      return stripExcluded(base);
    case "podOverrides":
      return base.podOverrides;
    case "ingressSpec":
      return base.ingressSpec;
    case "miteSpec":
      return base.miteSpec;
    case "statefulSet":
      return typeof ro?.statefulSet === "string" ? (ro.statefulSet as string).trim() : undefined;
    case "service":
      return typeof ro?.service === "string" ? (ro.service as string).trim() : undefined;
    case "ingress":
      return typeof ro?.ingress === "string" ? (ro.ingress as string).trim() : undefined;
  }
}

interface SpecEditorCardProps {
  cluster: string;
  ns: string;
  name: string;
  /** The controller's full spec, projected by the BFF. Hydrates every tier. */
  spec?: ControllerSpec;
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
  spec,
  initialOverlay,
  initialPodOverrides,
  initialIngressSpec,
  initialMiteSpec,
  canUpdate,
}: SpecEditorCardProps) {
  const { toast } = useToast();
  const qc = useQueryClient();
  const { data: openapiRoot } = useOpenAPISchema();

  // ── Tier 1: spec form state (curated draft) ──
  const [specValue, setSpecValue] = useState<Record<string, unknown>>({});
  // Incremented whenever the curated draft is replaced by hydration. Used as
  // the form's `key` so RJSF remounts it with the REAL formData: RJSF's
  // ObjectField seeds its additional-property row order ONCE at mount
  // (useState(() => getAdditionalPropertyOrder(...))) and never re-syncs it,
  // so a draft that hydrates after mount — SpecEditorCard always mounts the
  // form with {} and hydrates in an effect — would otherwise never render its
  // map rows. Keying on this counter (never on the draft) remounts ONLY when
  // the draft is replaced; a user keystroke leaves it unchanged, so focus is
  // retained while typing.
  const [formHydrationKey, setFormHydrationKey] = useState(0);
  // Live validation errors lifted from the curated form's onChange (§9). They
  // gate Save by containment against the curated patch (§2); cleared whenever
  // the form tier is re-hydrated, because a replaced draft has no known errors
  // until it is edited again.
  const [formErrors, setFormErrors] = useState<RJSFValidationError[]>([]);

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

  // ── Immutable baseline + per-tier draft versions + hydration snapshots ──
  // The baseline is the last known server state. Every tier diffs against its
  // OWN hydration snapshot on save, and the baseline is rebased (never the
  // drafts) on a background refetch while a draft is dirty, and on save
  // success. A dirty tier's snapshot stays put — the value its draft was
  // hydrated from — so a save never re-emits a field that only the server
  // changed in a background refetch.
  const [specBaseline, setSpecBaseline] = useState<Record<string, unknown>>({});
  const [draftVersion, setDraftVersion] = useState<Record<DraftTier, number>>(ZERO_VERSIONS);
  const [tierSnapshot, setTierSnapshot] = useState<Partial<Record<DraftTier, unknown>>>({});

  // Latest-value refs so the async save path can read state that may have moved
  // on since the render that started the save.
  const draftVersionRef = useRef(draftVersion);
  draftVersionRef.current = draftVersion;
  const specBaselineRef = useRef(specBaseline);
  specBaselineRef.current = specBaseline;
  const tierSnapshotRef = useRef(tierSnapshot);
  tierSnapshotRef.current = tierSnapshot;

  const isDirty = (tier: DraftTier) => draftVersionRef.current[tier] > 0;

  // A map-key rename lives in MapEntry's local state until blur, so typing a
  // new key never changes specValue and never fires RJSF onChange — the form
  // tier would otherwise look pristine while a rename is in flight, and a
  // background refetch would re-hydrate it (remounting the form and dropping
  // the typed key and focus). Reporting the edit as in-flight bumps the form
  // draft version once, so the existing "dirty tiers are skipped on refetch"
  // machinery keeps the form mounted. The version is never decremented here —
  // the same convention as every other form touch, which stays dirty until a
  // successful save re-hydrates the tier (a committed rename additionally
  // bumps it again through RJSF onChange).
  const reportFormKeyEditing = useCallback((editing: boolean) => {
    if (editing) {
      setDraftVersion((p) => ({ ...p, form: p.form + 1 }));
    }
  }, []);

  // The snapshot a tier diffs against on save. A tier that has never been
  // hydrated falls back to the live baseline's extraction (defensive — the
  // card only saves after hydration). Presence is the signal, not the value:
  // a tier hydrated while its field was absent records `undefined` as its
  // snapshot, which must NOT fall back to the live baseline — a dirty tier's
  // save would otherwise re-emit a field another writer just added as a null
  // removal.
  const snapshotFor = (tier: DraftTier): unknown => {
    const snapshots = tierSnapshotRef.current;
    return Object.prototype.hasOwnProperty.call(snapshots, tier)
      ? snapshots[tier]
      : tierBaseValue(tier, specBaselineRef.current);
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
  // The patch being attempted, captured when the save started, so Override can
  // resend it with force rather than rebuild it from current state.
  const pendingPatch = useRef<Record<string, unknown> | null>(null);
  // The draft versions captured when the save started — the rebase after an
  // Override must compare against these, not a fresh capture.
  const pendingVersions = useRef<Record<DraftTier, number>>(ZERO_VERSIONS);
  // Set by Reload to force a full re-hydration on the next refetch, overriding
  // the refetch-while-dirty rule that would otherwise leave drafts untouched.
  const forceHydrateRef = useRef(false);

  // ── PodOverrides schema for Tier 2 validation ──
  const podOverridesSchema = openapiRoot ? getPodOverridesSchema(openapiRoot) : undefined;
  const ingressSpecSchema = openapiRoot ? getIngressSpecSchema(openapiRoot) : undefined;
  const miteSpecSchema = openapiRoot ? getMiteSpecSchema(openapiRoot) : undefined;

  // ── Hydration & rebase ─────────────────────────────────────────────────────
  // Hydrate a single tier's draft from a baseline, mark it pristine, and
  // record the baseline value it was hydrated from as its snapshot (the save
  // diff compares the draft against this snapshot, not the live baseline). The
  // curated tier hydrates from the stripped baseline; the form is remounted on
  // each hydration (see formHydrationKey) so map rows render from real data,
  // and the save diff still runs against the hydrated draft, never against
  // what RJSF happens to render.
  const hydrateTier = (tier: DraftTier, base: Record<string, unknown>) => {
    const value = tierDraftValue(tier, base);
    switch (tier) {
      case "form":
        setSpecValue(value as Record<string, unknown>);
        // The draft was replaced — old validation errors no longer describe it.
        setFormErrors([]);
        // Bump the remount key so RJSF re-seeds its additional-property rows
        // from the fresh draft (see formHydrationKey above) — but ONLY when
        // the draft actually changed. A pristine tier re-hydrated with an
        // identical value (a refetch that only moved a YAML tier) must not
        // remount: that would drop focus/cursor from a field the user has
        // focused but not yet typed in.
        if (!deepEqual(value, specValue)) {
          setFormHydrationKey((n) => n + 1);
        }
        break;
      case "podOverrides":
        setPodOverridesText(rebasedYamlText(podOverridesText, base.podOverrides));
        break;
      case "ingressSpec":
        setIngressSpecText(rebasedYamlText(ingressSpecText, base.ingressSpec));
        break;
      case "miteSpec":
        setMiteSpecText(rebasedYamlText(miteSpecText, base.miteSpec));
        break;
      case "statefulSet":
        setStatefulSet(value as string);
        break;
      case "service":
        setService(value as string);
        break;
      case "ingress":
        setIngress(value as string);
        break;
    }
    setDraftVersion((v) => ({ ...v, [tier]: 0 }));
    setTierSnapshot((s) => ({ ...s, [tier]: tierBaseValue(tier, base) }));
  };

  const hydrateAll = (base: Record<string, unknown>) => {
    for (const tier of DRAFT_TIERS) hydrateTier(tier, base);
  };

  const identity = `${cluster}/${ns}/${name}`;
  const prevIdentity = useRef<string | null>(null);
  const prevSpec = useRef<ControllerSpec | undefined>(undefined);

  useEffect(() => {
    if (!spec) {
      // A transient undefined spec must not leave a pending forced reset
      // behind: consumed on a later unrelated refetch, it would wipe a draft
      // the user has since dirtied. (In production the card only mounts after
      // the controller detail loads, so this is a defensive fallback.)
      forceHydrateRef.current = false;
      return;
    }
    const base = spec as unknown as Record<string, unknown>;
    const idChanged = prevIdentity.current !== identity;
    const specChanged = prevSpec.current !== spec;
    prevIdentity.current = identity;
    prevSpec.current = spec;
    if (!specChanged && !idChanged) return;

    // Conflict Reload forced a full reset regardless of dirtiness.
    if (forceHydrateRef.current) {
      forceHydrateRef.current = false;
      setSpecBaseline(base);
      hydrateAll(base);
      return;
    }

    if (idChanged || !DRAFT_TIERS.some(isDirty)) {
      // Initial load, identity change, or a pristine background refetch:
      // hydrate the baseline AND every draft.
      setSpecBaseline(base);
      hydrateAll(base);
    } else {
      // Background refetch while at least one draft is dirty: update the
      // baseline only, and rehydrate the pristine tiers (they have no edits to
      // lose) while leaving the dirty drafts untouched.
      setSpecBaseline(base);
      for (const tier of DRAFT_TIERS) {
        if (!isDirty(tier)) hydrateTier(tier, base);
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [spec, identity]);

  // ── Save gate (§2): block Save when an invalid field path is at or below the
  // curated patch, or fails to resolve to a real form value at all (fail
  // closed). Recomputes when the draft, its live errors, or the tier's
  // hydration snapshot change. The snapshot drives the same diff `doSave`
  // builds, so the render-time gate and the save-time guard cannot diverge.
  const saveBlockedPaths = useMemo(() => {
    const curatedPatch = diffValues(snapshotFor("form"), specValue);
    return blockingErrorPaths(formErrors, specValue, curatedPatch);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [formErrors, specValue, tierSnapshot, specBaseline]);

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

  // ── Save path ──────────────────────────────────────────────────────────────
  // Send one patch and handle the response: on success rebase the baseline and
  // every tier whose draft version is unchanged since the save started; on a
  // non-conflict failure leave baseline and drafts untouched; on a conflict
  // stash the patch + versions so Override can resend with force.
  const sendPatch = async (
    patch: Record<string, unknown>,
    savedVersions: Record<DraftTier, number>,
    force: boolean,
  ) => {
    setSaving(true);
    try {
      pendingPatch.current = patch;
      pendingVersions.current = savedVersions;
      const updated = await updateController(cluster, name, ns, patch, { force });
      setShowConflict(false);
      setConflicts([]);
      // The save succeeded. A requested removal (explicit null) only takes
      // effect for a leaf this manager can release; where another field
      // manager owns it, server-side apply retains it and the server reports
      // it in unappliedRemovals. Surface that as a NON-BLOCKING notice naming
      // the field(s) instead of an unqualified success — the save did not
      // fail, so this must not render as an error.
      toast(unappliedRemovalNotice(updated) ?? "Spec saved");

      // Rebase. Only tiers whose version is unchanged since the save started
      // are reset to the returned spec; a tier edited while the save was in
      // flight keeps its draft and stays dirty.
      const patchSpec = (patch.spec as Record<string, unknown> | undefined) ?? {};
      const newBaseline =
        (updated?.spec as unknown as Record<string, unknown> | undefined) ??
        applyPatchToObject(specBaselineRef.current, patchSpec);
      setSpecBaseline(newBaseline);
      for (const tier of DRAFT_TIERS) {
        if (draftVersionRef.current[tier] === savedVersions[tier]) {
          hydrateTier(tier, newBaseline);
        }
      }

      qc.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
    } catch (e) {
      if (e instanceof ControllerConflictError) {
        setConflicts(e.conflicts);
        setShowConflict(true);
      } else {
        // Non-conflict failure: baseline and every draft stay unchanged.
        toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
      }
    } finally {
      setSaving(false);
    }
  };

  const doSave = async (force: boolean) => {
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

    // Build the patch as a real recursive diff of each tier against ITS OWN
    // hydration snapshot — the baseline value that tier was last hydrated
    // from, not the live specBaseline. A background refetch may have advanced
    // the live baseline while this tier sat dirty; diffing against the live
    // baseline would report every field the SERVER changed as a local edit and
    // re-send the stale draft value, silently reverting the other writer's
    // change. Diffing against the snapshot emits exactly what the user
    // changed. The curated tier diffs its hydrated draft — not what RJSF
    // happens to render — so the full stripped value is always compared, never
    // a render-layer projection.
    const specPatch: Record<string, unknown> = {};

    const curatedPatch = diffValues(snapshotFor("form"), specValue);
    if (curatedPatch !== NO_CHANGE) {
      Object.assign(specPatch, curatedPatch);
    }

    // §2 save gate. The button is disabled in render when blocked, so this is a
    // second line of defence for any non-click save path — and it is why
    // Reload-or-Override is unaffected: Override resends `pendingPatch.current`
    // straight through `sendPatch`, never through this check.
    if (blockingErrorPaths(formErrors, specValue, curatedPatch).length > 0) {
      return;
    }

    for (const tier of [
      { key: "podOverrides" as DraftTier },
      { key: "ingressSpec" as DraftTier },
      { key: "miteSpec" as DraftTier },
    ]) {
      const tierPatch = diffValues(
        snapshotFor(tier.key),
        parsedTiers[tier.key],
      );
      if (tierPatch !== NO_CHANGE) specPatch[tier.key] = tierPatch;
    }

    // Tier 3: resourceOverlay — each overlay is a raw YAML string, so the diff
    // compares the (whitespace-normalised) text against the tier's snapshot.
    const roBase: Record<string, unknown> = {};
    for (const key of OVERLAY_KEYS) {
      roBase[key] = snapshotFor(key);
    }
    const roDraft: Record<string, unknown> = {
      statefulSet: statefulSet.trim() || undefined,
      service: service.trim() || undefined,
      ingress: ingress.trim() || undefined,
    };
    const roPatch = diffValues(roBase, roDraft);
    if (roPatch !== NO_CHANGE) specPatch.resourceOverlay = roPatch;

    // A save with no differences issues no request.
    if (Object.keys(specPatch).length === 0) return;

    const savedVersions = { ...draftVersionRef.current };
    await sendPatch({ spec: specPatch }, savedVersions, force);
  };

  const handleSave = () => {
    void doSave(false);
  };

  // Conflict Override resends the CAPTURED configuration with force — not a
  // patch rebuilt from current state.
  const handleOverride = () => {
    if (pendingPatch.current) {
      void sendPatch(pendingPatch.current, pendingVersions.current, true);
    }
  };

  // Conflict Reload must reset the baseline AND every draft from freshly
  // fetched data. The React Query cache holds the last successful fetch —
  // hydrate every tier from it immediately, then invalidate to refetch. The
  // effect can't be relied on alone: a refetch whose data is structurally
  // identical reuses the same `spec` reference (TanStack structural sharing),
  // so the hydration effect would never re-run. forceHydrateRef covers the
  // case where the cache is empty.
  const handleReload = () => {
    setShowConflict(false);
    setConflicts([]);
    const fresh = qc.getQueryData<{ spec?: ControllerSpec }>([
      "controller",
      cluster,
      ns,
      name,
    ]);
    if (fresh?.spec) {
      setSpecBaseline(fresh.spec as unknown as Record<string, unknown>);
      hydrateAll(fresh.spec as unknown as Record<string, unknown>);
    } else {
      forceHydrateRef.current = true;
    }
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
            <KeyEditContext.Provider value={reportFormKeyEditing}>
              <ControllerSpecForm
                key={formHydrationKey}
                value={specValue}
                onChange={(v) => {
                  setSpecValue(v);
                  setDraftVersion((p) => ({ ...p, form: p.form + 1 }));
                }}
                onErrorsChange={setFormErrors}
              />
            </KeyEditContext.Provider>
          )}

          {/* Tier 2: Pod overrides YAML editor */}
          {activeTab === "podOverrides" && (
            <div className={styles.yamlTier}>
              <div className={styles.tierLabel}>Pod overrides (YAML)</div>
              <YamlTierEditor
                value={podOverridesText}
                onChange={(v) => {
                  setPodOverridesText(v);
                  setDraftVersion((p) => ({ ...p, podOverrides: p.podOverrides + 1 }));
                }}
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
                onChange={(v) => {
                  setIngressSpecText(v);
                  setDraftVersion((p) => ({ ...p, ingressSpec: p.ingressSpec + 1 }));
                }}
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
                onChange={(v) => {
                  setMiteSpecText(v);
                  setDraftVersion((p) => ({ ...p, miteSpec: p.miteSpec + 1 }));
                }}
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
                  onChange={(v: string) => {
                    const kind = overlaySubTab as OverlayResourceKind;
                    overlaySetters[kind](v);
                    setDraftVersion((p) => ({ ...p, [kind]: p[kind] + 1 }));
                  }}
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
            {saveBlockedPaths.length > 0 && (
              <div className={styles.errorBanner}>
                Cannot save: invalid value at {saveBlockedPaths.join(", ")}. Fix
                the field{saveBlockedPaths.length > 1 ? "s" : ""} to save.
              </div>
            )}
            <Button
              size="sm"
              variant="primary"
              disabled={saving || saveBlockedPaths.length > 0}
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
