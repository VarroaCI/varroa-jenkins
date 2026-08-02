import { useState, useCallback, useEffect, useRef, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { controllerRoute } from "../routing";
import { parse as parseYAML } from "yaml";
import type {
  ProvisioningConfig,
  ComposedBundleSpec,
  ComposedBundle,
  ComposedInput,
  RBACGroupBinding,
  PreflightCheck,
  CatalogItemSummary,
  CatalogVariable,
  ReconciliationMode,
  PodOverrides,
  ResourceOverlay,
  OverlayResourceKind,
  PreviewResponse,
  ProbeSpec,
  ProbesSpec,
} from "../types";
import { PROBE_DEFAULTS } from "../types";
import {
  getProvisioningConfig,
  getDeployableNamespaces,
  getController,
  listComposedBundles,
  listCatalogItems,
  previewComposedBundle,
  previewControllerOverlay,
  preflightController,
  renderController,
  createController,
  controllerEventsUrl,
  listGroups,
} from "../api/client";
import { useClusters } from "../hooks/useClusters";
import { bffFetch } from "../hooks/useApi";
import type { GroupEntry, DeployableNamespaces } from "../api/client";
import { OverlayEditor, DiffView, OVERLAY_RESOURCES, type OverlayFieldError } from "../components/OverlayEditor";
import VersionPicker from "../components/VersionPicker";
import styles from "./ControllerWizard.module.css";

const STEPS = [
  { label: "Basics", sub: "name · namespace · version" },
  { label: "Configuration bundle", sub: "catalog items · JCasC" },
  { label: "Resources & access", sub: "size · RBAC · ingress" },
  { label: "Advanced options", sub: "power · reconciliation · overrides" },
  { label: "Review & deploy", sub: "manifest · checks" },
] as const;

const TOTAL_STEPS = STEPS.length;
const DNS1123_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

const K8S_CPU_RE = /^\d+m?$/;
const K8S_QUANTITY_RE = /^\d+(\.\d+)?(m|[kKMGTPE]i?)?$/;
const BUILTIN_ROLES = ["admin", "operator", "developer", "viewer"];
const INTEGER_RE = /^-?\d+$/;

function isValidOptionalInteger(text: string): boolean {
  return text.trim() === "" || INTEGER_RE.test(text.trim());
}

// Only returns a value when text is a valid integer; NaN/non-numeric input
// (e.g. from a stray Number(...) call) must never reach JSON.stringify, since
// it serializes to null and the Go decoder then silently zeroes the field
// instead of rejecting the request.
function parseOptionalInt(text: string): number | undefined {
  const t = text.trim();
  if (!INTEGER_RE.test(t)) return undefined;
  return Number(t);
}

function isValidOptionalYAML(text: string): boolean {
  if (!text.trim()) return true;
  try {
    parseYAML(text);
    return true;
  } catch {
    return false;
  }
}

const PROBE_KINDS = ["startup", "readiness", "liveness"] as const;
type ProbeKind = (typeof PROBE_KINDS)[number];

interface ProbeFormState {
  enabled: boolean;
  initialDelaySeconds: string;
  periodSeconds: string;
  timeoutSeconds: string;
  failureThreshold: string;
  successThreshold: string;
}

type ProbesFormState = Record<ProbeKind, ProbeFormState>;

function emptyProbeForm(spec?: ProbeSpec): ProbeFormState {
  return {
    enabled: spec?.disabled !== true,
    initialDelaySeconds: spec?.initialDelaySeconds != null ? String(spec.initialDelaySeconds) : "",
    periodSeconds: spec?.periodSeconds != null ? String(spec.periodSeconds) : "",
    timeoutSeconds: spec?.timeoutSeconds != null ? String(spec.timeoutSeconds) : "",
    failureThreshold: spec?.failureThreshold != null ? String(spec.failureThreshold) : "",
    successThreshold: spec?.successThreshold != null ? String(spec.successThreshold) : "",
  };
}

function emptyProbesForm(probes?: ProbesSpec): ProbesFormState {
  return {
    startup: emptyProbeForm(probes?.startup),
    readiness: emptyProbeForm(probes?.readiness),
    liveness: emptyProbeForm(probes?.liveness),
  };
}

function parseOptionalProbeNumber(text: string): number | undefined {
  const trimmed = text.trim();
  if (!trimmed) return undefined;
  const value = Number(trimmed);
  // Probe timing fields are *int32 server-side: reject non-integer/negative
  // input so a stray "1.5" never becomes a 400 on JSON unmarshalling.
  return Number.isInteger(value) && value >= 0 ? value : undefined;
}

function buildProbeSpec(form: ProbeFormState): ProbeSpec | undefined {
  if (!form.enabled) {
    return { disabled: true };
  }
  const probe: ProbeSpec = {};
  const initialDelaySeconds = parseOptionalProbeNumber(form.initialDelaySeconds);
  if (initialDelaySeconds !== undefined) probe.initialDelaySeconds = initialDelaySeconds;
  const periodSeconds = parseOptionalProbeNumber(form.periodSeconds);
  if (periodSeconds !== undefined) probe.periodSeconds = periodSeconds;
  const timeoutSeconds = parseOptionalProbeNumber(form.timeoutSeconds);
  if (timeoutSeconds !== undefined) probe.timeoutSeconds = timeoutSeconds;
  const failureThreshold = parseOptionalProbeNumber(form.failureThreshold);
  if (failureThreshold !== undefined) probe.failureThreshold = failureThreshold;
  const successThreshold = parseOptionalProbeNumber(form.successThreshold);
  if (successThreshold !== undefined) probe.successThreshold = successThreshold;
  return Object.keys(probe).length > 0 ? probe : undefined;
}

function buildProbesSpec(forms: ProbesFormState): ProbesSpec | undefined {
  const probes: ProbesSpec = {};
  for (const kind of PROBE_KINDS) {
    const probe = buildProbeSpec(forms[kind]);
    if (probe) {
      probes[kind] = probe;
    }
  }
  return Object.keys(probes).length > 0 ? probes : undefined;
}

function titleCaseProbe(kind: ProbeKind): string {
  return kind[0].toUpperCase() + kind.slice(1);
}

function ProbePanel({
  kind,
  form,
  onChange,
}: {
  kind: ProbeKind;
  form: ProbeFormState;
  onChange: (next: ProbeFormState) => void;
}) {
  const label = titleCaseProbe(kind);
  const defaults = PROBE_DEFAULTS[kind];
  const update = (patch: Partial<ProbeFormState>) => onChange({ ...form, ...patch });
  return (
    <div className={styles.field}>
      <div className={styles.grpRow}>
        <input
          id={`probe-${kind}-enabled`}
          type="checkbox"
          checked={form.enabled}
          onChange={(e) => update({ enabled: e.target.checked })}
        />
        <label htmlFor={`probe-${kind}-enabled`}>Enable {label.toLowerCase()} probe</label>
      </div>
      <div className={`${styles.frow3} ${styles.mt8}`}>
        <div>
          <label htmlFor={`probe-${kind}-initialDelaySeconds`}>{label} initial delay seconds</label>
          <input
            id={`probe-${kind}-initialDelaySeconds`}
            type="number"
            placeholder={String(defaults.initialDelaySeconds)}
            value={form.initialDelaySeconds}
            onChange={(e) => update({ initialDelaySeconds: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
          />
        </div>
        <div>
          <label htmlFor={`probe-${kind}-periodSeconds`}>{label} period seconds</label>
          <input
            id={`probe-${kind}-periodSeconds`}
            type="number"
            placeholder={String(defaults.periodSeconds)}
            value={form.periodSeconds}
            onChange={(e) => update({ periodSeconds: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
          />
        </div>
        <div>
          <label htmlFor={`probe-${kind}-timeoutSeconds`}>{label} timeout seconds</label>
          <input
            id={`probe-${kind}-timeoutSeconds`}
            type="number"
            placeholder={String(defaults.timeoutSeconds)}
            value={form.timeoutSeconds}
            onChange={(e) => update({ timeoutSeconds: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
          />
        </div>
      </div>
      <div className={`${styles.frow3} ${styles.mt8}`}>
        <div>
          <label htmlFor={`probe-${kind}-failureThreshold`}>{label} failure threshold</label>
          <input
            id={`probe-${kind}-failureThreshold`}
            type="number"
            placeholder={String(defaults.failureThreshold)}
            value={form.failureThreshold}
            onChange={(e) => update({ failureThreshold: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
          />
        </div>
        <div>
          <label htmlFor={`probe-${kind}-successThreshold`}>{label} success threshold</label>
          <input
            id={`probe-${kind}-successThreshold`}
            type="number"
            placeholder={String(defaults.successThreshold)}
            value={form.successThreshold}
            onChange={(e) => update({ successThreshold: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
          />
        </div>
      </div>
    </div>
  );
}

const TYPE_ICONS: Record<string, { cls: string; label: string }> = {
  jcasc: { cls: styles.tJcasc, label: "\u2699" },
  plugin: { cls: styles.tPlugin, label: "\u25C6" },
  podtemplate: { cls: styles.tPodtemplate, label: "\u2638" },
  rbac: { cls: styles.tRbac, label: "\u2687" },
  item: { cls: styles.tItem, label: "\u25CB" },
  git: { cls: styles.tGit, label: "\u2387" },
  "pipeline-template": { cls: styles.tItem, label: "\u25B6" },
};

const TIMELINE_STEPS = [
  { event: "provisioning.started", label: "Provisioning", sub: "StatefulSet · PVC · init script · agent RBAC" },
  { event: "provisioning.completed", label: "Bundle delivered", sub: "composed JCasC + plugins mounted, vars resolved" },
  { event: "connected", label: "Mite handshake", sub: "bootstrap token → mTLS cert → command stream" },
  { event: "connected-done", label: "Connected", sub: "heartbeats streaming · Jenkins ready" },
];

export interface WizardDraft {
  cluster: string;
  name: string;
  namespace: string;
  version: string;
  description: string;
  bundleMode: "existing" | "compose" | "none";
  existingBundleName: string | null;
  existingBundleNamespace: string;
  composeSpec: ComposedBundleSpec | null;
  composeVariables: Record<string, string>;
  cpu: string;
  memory: string;
  storage: string;
  ingressHost: string;
  ingressMode: "" | "subdomain" | "path";
  ingressTlsSecretName: string;
  ingressAnnotations: Record<string, string>;
  ingressClassName: string;
  rbacGroups: RBACGroupBinding[];
  config: ProvisioningConfig | null;
  powerState: "" | "Stopped" | "Hibernated";
  reconciliationMode: ReconciliationMode | "";
  reconciliationInterval: string;
  reconciliationMaxDeferSeconds: string;
  reconciliationDrainTimeoutSeconds: string;
  reconciliationRolloutWave: string;
  miteCpu: string;
  miteMemory: string;
  miteImage: string;
  miteImagePullPolicy: string;
  className: string;
  backupEnabled: boolean;
  backupSchedule: string;
  backupRetentionDays: string;
  probes: ProbesFormState;
  podOverridesText: string;
  resourceOverlay: ResourceOverlay;
}

const EMPTY_DRAFT: WizardDraft = {
  cluster: "",
  name: "",
  namespace: "",
  version: "",
  description: "",
  bundleMode: "existing",
  existingBundleName: null,
  existingBundleNamespace: "",
  composeSpec: null,
  composeVariables: {},
  cpu: "",
  memory: "",
  storage: "",
  ingressHost: "",
  ingressMode: "",
  ingressTlsSecretName: "",
  ingressAnnotations: {},
  ingressClassName: "",
  rbacGroups: [],
  config: null,
  powerState: "",
  reconciliationMode: "",
  reconciliationInterval: "",
  reconciliationMaxDeferSeconds: "",
  reconciliationDrainTimeoutSeconds: "",
  reconciliationRolloutWave: "",
  miteCpu: "",
  miteMemory: "",
  miteImage: "",
  miteImagePullPolicy: "",
  className: "",
  backupEnabled: false,
  backupSchedule: "",
  backupRetentionDays: "",
  probes: emptyProbesForm(),
  podOverridesText: "",
  resourceOverlay: {},
};

export default function ControllerWizard() {
  const navigate = useNavigate();
  const { data: clusters, isError: clustersError } = useClusters();
  const activeClusters = clusters?.filter((c) => c.state === "active");
  const [step, setStep] = useState(1);
  const [draft, setDraft] = useState<WizardDraft>(EMPTY_DRAFT);
  const [configLoading, setConfigLoading] = useState(true);
  const [deployableNs, setDeployableNs] = useState<DeployableNamespaces | null>(null);
  const [bundles, setBundles] = useState<ComposedBundle[]>([]);
  const [bundleSearch, setBundleSearch] = useState("");
  const [groups, setGroups] = useState<GroupEntry[]>([]);
  const [nameCollision, setNameCollision] = useState(false);
  const collisionTimer = useRef<ReturnType<typeof setTimeout>>();
  const [previewOutput, setPreviewOutput] = useState<string>("");
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [unresolvedVars, setUnresolvedVars] = useState<string[]>([]);
  const [previewMissing, setPreviewMissing] = useState<string[]>([]);
  const [previewDrifted, setPreviewDrifted] = useState<string[]>([]);
  const [previewWarnings, setPreviewWarnings] = useState<string[]>([]);
  const [preflightChecks, setPreflightChecks] = useState<PreflightCheck[]>([]);
  const [preflightLoading, setPreflightLoading] = useState(false);
  const [deploying, setDeploying] = useState(false);
  const [deployed, setDeployed] = useState(false);
  const [deployError, setDeployError] = useState<string | null>(null);
  const [timelineIdx, setTimelineIdx] = useState(-1);
  const [catalogItems, setCatalogItems] = useState<CatalogItemSummary[]>([]);
  const [catalogOperatorNs, setCatalogOperatorNs] = useState<string>("");
  const [catalogSearch, setCatalogSearch] = useState("");
  const [showCatalogPicker, setShowCatalogPicker] = useState(false);
  const [showGitForm, setShowGitForm] = useState(false);
  const [gitUrl, setGitUrl] = useState("");
  const [gitPath, setGitPath] = useState("");
  const [gitRevision, setGitRevision] = useState("");
  const [groupTypeahead, setGroupTypeahead] = useState("");
  const [showGroupDropdown, setShowGroupDropdown] = useState(false);

  const [overlayPreview, setOverlayPreview] = useState<PreviewResponse | null>(null);
  const [overlayPreviewError, setOverlayPreviewError] = useState<string | null>(null);
  const [overlayFieldError, setOverlayFieldError] = useState<OverlayFieldError | null>(null);
  const [overlayPreviewing, setOverlayPreviewing] = useState(false);
  const [annotationKeyDraft, setAnnotationKeyDraft] = useState("");
  const [annotationValueDraft, setAnnotationValueDraft] = useState("");

  const updateDraft = useCallback((patch: Partial<WizardDraft>) => {
    setDraft((prev) => ({ ...prev, ...patch }));
  }, []);

  const canProceed = useCallback((): boolean => {
    switch (step) {
      case 1:
        return DNS1123_RE.test(draft.name) && draft.namespace.trim().length > 0 && draft.cluster.trim().length > 0;
      case 2:
        if (draft.bundleMode === "existing") return draft.existingBundleName !== null;
        if (draft.bundleMode === "compose") return (draft.composeSpec?.inputs?.length ?? 0) > 0;
        return true;
      case 3:
        return (
          K8S_CPU_RE.test(draft.cpu.trim()) &&
          K8S_QUANTITY_RE.test(draft.memory.trim()) &&
          K8S_QUANTITY_RE.test(draft.storage.trim())
        );
      case 4:
        return (
          isValidOptionalYAML(draft.podOverridesText) &&
          isValidOptionalInteger(draft.reconciliationMaxDeferSeconds) &&
          isValidOptionalInteger(draft.reconciliationDrainTimeoutSeconds) &&
          isValidOptionalInteger(draft.reconciliationRolloutWave) &&
          isValidOptionalInteger(draft.backupRetentionDays)
        );
      case 5:
        return preflightChecks.length > 0 && !preflightChecks.some((c) => c.status === "fail");
      default:
        return false;
    }
  }, [step, draft, preflightChecks]);

  const canProceedForStep = useCallback(
    (s: number): boolean => {
      switch (s) {
        case 1:
          return DNS1123_RE.test(draft.name) && draft.namespace.trim().length > 0 && draft.cluster.trim().length > 0;
        case 2:
          if (draft.bundleMode === "existing") return draft.existingBundleName !== null;
          if (draft.bundleMode === "compose") return (draft.composeSpec?.inputs?.length ?? 0) > 0;
          return true;
        case 3:
          return draft.cpu.trim().length > 0 && draft.memory.trim().length > 0 && draft.storage.trim().length > 0;
        default:
          return true;
      }
    },
    [draft]
  );

  const handleNext = () => {
    if (step < TOTAL_STEPS) setStep((s) => s + 1);
  };

  const handleBack = () => {
    if (step > 1) setStep((s) => s - 1);
  };

  const canReachStep = useCallback(
    (n: number): boolean => {
      if (n <= step) return true;
      for (let s = step; s < n; s++) {
        if (!canProceedForStep(s)) return false;
      }
      return true;
    },
    [step, canProceedForStep]
  );

  const goStep = (n: number) => {
    if (n < 1 || n > TOTAL_STEPS) return;
    if (!canReachStep(n)) return;
    setStep(n);
  };

  useEffect(() => {
    const cluster = draft.cluster || "core";
    getProvisioningConfig(cluster)
      .then((cfg) => {
        setDraft((prev) => ({
          ...prev,
          config: cfg,
          version: prev.version || cfg.defaultVersion,
        }));
      })
      .catch(() => {})
      .finally(() => {
        setConfigLoading(false);
      });

    listComposedBundles(cluster)
      .then((data) => setBundles(data.items ?? []))
      .catch(() => {});

    listGroups()
      .then((data) => setGroups(data))
      .catch(() => {});

    listCatalogItems(cluster, {})
      .then((data) => {
        setCatalogItems(data.items ?? []);
        setCatalogOperatorNs(data.operatorNamespace ?? "");
      })
      .catch(() => {});
  }, [draft.cluster]);

  useEffect(() => {
    return () => {
      if (collisionTimer.current) clearTimeout(collisionTimer.current);
    };
  }, []);

  // Seed cluster from useClusters when it resolves; only active clusters.
  useEffect(() => {
    if (activeClusters && !draft.cluster) {
      const h = activeClusters.find((c) => c.core);
      if (h) {
        setDraft((prev) => ({ ...prev, cluster: h.name }));
      }
    }
  }, [activeClusters, draft.cluster]);

  // Fetch deployable namespaces when cluster changes.
  useEffect(() => {
    if (!draft.cluster) return;
    setDeployableNs(null); // disable the field while loading
    getDeployableNamespaces(draft.cluster)
      .then((dns) => {
        setDeployableNs(dns);
        // Reseed on cluster switch: keep a still-valid selection; replace an
        // empty or now-invalid one. Freeform text survives when allowed.
        setDraft((prev) => {
          const keep = prev.namespace !== "" &&
            (dns.allowFreeform || dns.namespaces.includes(prev.namespace));
          return keep ? prev : { ...prev, namespace: dns.defaultNamespace };
        });
      })
      .catch(() =>
        setDeployableNs({ namespaces: [], defaultNamespace: "", allowFreeform: false, degraded: true })
      );
  }, [draft.cluster]);

  const debouncedNameCheck = useCallback(
    (name: string, ns: string) => {
      if (collisionTimer.current) clearTimeout(collisionTimer.current);
      if (!name || !DNS1123_RE.test(name)) {
        setNameCollision(false);
        return;
      }
      collisionTimer.current = setTimeout(() => {
        if (!ns) return;
        getController(draft.cluster, name, ns)
          .then(() => setNameCollision(true))
          .catch(() => setNameCollision(false));
      }, 400);
    },
    []
  );

  const filteredBundles = useMemo(() => {
    if (!bundleSearch.trim()) return bundles;
    const q = bundleSearch.toLowerCase();
    return bundles.filter(
      (b) =>
        b.metadata.name.toLowerCase().includes(q) ||
        (b.spec.displayName ?? "").toLowerCase().includes(q)
    );
  }, [bundles, bundleSearch]);

  const filteredGroups = useMemo(() => {
    if (!groupTypeahead.trim()) return groups;
    const q = groupTypeahead.toLowerCase();
    return groups.filter((g) => g.name.toLowerCase().includes(q));
  }, [groups, groupTypeahead]);

  const derivedHost = useMemo(() => {
    if (draft.ingressHost.trim()) return draft.ingressHost;
    const root = draft.config?.rootDomain;
    if (draft.name && root) return `${draft.name}.${root}`;
    return "";
  }, [draft.name, draft.ingressHost, draft.config]);

  const handlePreviewBundle = async () => {
    if (!draft.composeSpec) return;
    setPreviewError(null);
    setPreviewOutput("");
    setUnresolvedVars([]);
    setPreviewMissing([]);
    setPreviewDrifted([]);
    setPreviewWarnings([]);
    try {
      const result = await previewComposedBundle(draft.cluster || "core", draft.namespace || "default", draft.composeSpec);
      const combined = [
        result.jenkinsYaml && `# --- jenkins.yaml ---\n${result.jenkinsYaml}`,
        result.pluginsYaml && `# --- plugins.yaml ---\n${result.pluginsYaml}`,
        result.itemsYaml && `# --- items.yaml ---\n${result.itemsYaml}`,
        result.rbacYaml && `# --- rbac.yaml ---\n${result.rbacYaml}`,
      ]
        .filter(Boolean)
        .join("\n\n");
      setPreviewOutput(combined || "(empty)");
      // client.ts already normalizes null → [] for missing/drifted.
      setPreviewMissing(result.missing);
      setPreviewDrifted(result.drifted);
      setPreviewWarnings(result.warnings ?? []);
      if ((result as unknown as Record<string, unknown>).unresolvedVariables) {
        setUnresolvedVars((result as unknown as Record<string, unknown>).unresolvedVariables as string[]);
      }
    } catch (e: unknown) {
      setPreviewError(e instanceof Error ? e.message : "Preview failed");
    }
  };

  const addComposeInput = (item: CatalogItemSummary) => {
    const input: ComposedInput = {
      itemRef: { name: item.name, namespace: item.namespace },
    };
    setDraft((prev) => ({
      ...prev,
      composeSpec: {
        ...(prev.composeSpec ?? {}),
        inputs: [...(prev.composeSpec?.inputs ?? []), input],
      },
    }));
    setShowCatalogPicker(false);
    setCatalogSearch("");
  };

  const addGitInput = () => {
    if (!gitUrl.trim()) return;
    const input: ComposedInput = {
      gitSource: {
        repoURL: gitUrl.trim(),
        path: gitPath.trim() || "/",
        ...(gitRevision.trim() ? { revision: gitRevision.trim() } : {}),
      },
    };
    setDraft((prev) => ({
      ...prev,
      composeSpec: {
        ...(prev.composeSpec ?? {}),
        inputs: [...(prev.composeSpec?.inputs ?? []), input],
      },
    }));
    setShowGitForm(false);
    setGitUrl("");
    setGitPath("");
    setGitRevision("");
  };

  const removeComposeInput = (idx: number) => {
    setDraft((prev) => ({
      ...prev,
      composeSpec: {
        ...(prev.composeSpec ?? {}),
        inputs: (prev.composeSpec?.inputs ?? []).filter((_, i) => i !== idx),
      },
    }));
  };

  const addRbacGroup = (groupName: string) => {
    if (draft.rbacGroups.some((g) => g.name === groupName)) return;
    setDraft((prev) => ({
      ...prev,
      rbacGroups: [...prev.rbacGroups, { name: groupName, role: "viewer" }],
    }));
    setGroupTypeahead("");
    setShowGroupDropdown(false);
  };

  const removeRbacGroup = (idx: number) => {
    setDraft((prev) => ({
      ...prev,
      rbacGroups: prev.rbacGroups.filter((_, i) => i !== idx),
    }));
  };

  const updateRbacRole = (idx: number, role: string) => {
    setDraft((prev) => ({
      ...prev,
      rbacGroups: prev.rbacGroups.map((g, i) => (i === idx ? { ...g, role } : g)),
    }));
  };

  const addIngressAnnotation = () => {
    const key = annotationKeyDraft.trim();
    if (!key) return;
    setDraft((prev) => ({
      ...prev,
      ingressAnnotations: { ...prev.ingressAnnotations, [key]: annotationValueDraft },
    }));
    setAnnotationKeyDraft("");
    setAnnotationValueDraft("");
  };

  const removeIngressAnnotation = (key: string) => {
    setDraft((prev) => {
      const next = { ...prev.ingressAnnotations };
      delete next[key];
      return { ...prev, ingressAnnotations: next };
    });
  };

  // Explicit "Preview merge" button — unlike ControllerDetail's debounced live
  // preview, the wizard's controller doesn't exist yet, so there's no "live"
  // baseline to diff against; always preview against the base (overlay-stripped)
  // rendering.
  const handlePreviewOverlay = useCallback(async () => {
    setOverlayFieldError(null);
    setOverlayPreviewError(null);
    let podOverrides: PodOverrides | undefined;
    if (draft.podOverridesText.trim()) {
      try {
        podOverrides = parseYAML(draft.podOverridesText) as PodOverrides;
      } catch (e) {
        setOverlayFieldError({
          field: "podOverrides",
          message: `invalid YAML: ${e instanceof Error ? e.message : "parse error"}`,
        });
        return;
      }
    }
    setOverlayPreviewing(true);
    try {
      const res = await previewControllerOverlay(draft.cluster, draft.namespace, draft.name, {
        podOverrides,
        resourceOverlay: draft.resourceOverlay,
        baseline: "base",
      });
      setOverlayPreview(res);
    } catch (e) {
      const msg = e instanceof Error ? e.message : "preview failed";
      if (msg.startsWith("400")) {
        const m = msg.match(/"error"\s*:\s*"([^:"]+):\s*([^"]*)"/);
        if (m) setOverlayFieldError({ field: m[1].trim(), message: m[2].trim() });
        else setOverlayPreviewError(msg);
      } else {
        setOverlayPreviewError(msg);
      }
    } finally {
      setOverlayPreviewing(false);
    }
  }, [draft.namespace, draft.name, draft.podOverridesText, draft.resourceOverlay]);

  const handleRunPreflight = useCallback(async () => {
    setPreflightLoading(true);
    const body = buildSubmitBody();
    try {
      const result = await preflightController(draft.cluster, draft.namespace, body);
      setPreflightChecks(result.checks ?? []);
    } catch (e: unknown) {
      setPreflightChecks([]);
    } finally {
      setPreflightLoading(false);
    }
  }, [draft]);

  useEffect(() => {
    if (step === 5) {
      handleRunPreflight();
    }
  }, [step]);

  const buildSubmitBody = (): Record<string, unknown> => {
    const ingressSpec: Record<string, unknown> = {};
    if (draft.ingressHost) ingressSpec.host = draft.ingressHost;
    if (draft.ingressMode) ingressSpec.mode = draft.ingressMode;
    if (draft.ingressTlsSecretName.trim()) ingressSpec.tlsSecretName = draft.ingressTlsSecretName.trim();
    if (draft.ingressClassName.trim()) ingressSpec.ingressClassName = draft.ingressClassName.trim();
    if (Object.keys(draft.ingressAnnotations).length > 0) ingressSpec.annotations = draft.ingressAnnotations;

    const reconciliationPolicy: Record<string, unknown> = {};
    if (draft.reconciliationMode) reconciliationPolicy.mode = draft.reconciliationMode;
    if (draft.reconciliationInterval.trim()) reconciliationPolicy.interval = draft.reconciliationInterval.trim();
    const maxDeferSeconds = parseOptionalInt(draft.reconciliationMaxDeferSeconds);
    if (maxDeferSeconds !== undefined) reconciliationPolicy.maxDeferSeconds = maxDeferSeconds;
    const drainTimeoutSeconds = parseOptionalInt(draft.reconciliationDrainTimeoutSeconds);
    if (drainTimeoutSeconds !== undefined) reconciliationPolicy.drainTimeoutSeconds = drainTimeoutSeconds;
    const rolloutWave = parseOptionalInt(draft.reconciliationRolloutWave);
    if (rolloutWave !== undefined) reconciliationPolicy.rolloutWave = rolloutWave;

    const miteSpec: Record<string, unknown> = {};
    if (draft.miteCpu.trim() || draft.miteMemory.trim()) {
      miteSpec.resources = {
        requests: {
          ...(draft.miteCpu.trim() ? { cpu: draft.miteCpu.trim() } : {}),
          ...(draft.miteMemory.trim() ? { memory: draft.miteMemory.trim() } : {}),
        },
      };
    }
    if (draft.miteImage.trim()) {
      miteSpec.image = draft.miteImage.trim();
    }
    if (draft.miteImagePullPolicy) {
      miteSpec.imagePullPolicy = draft.miteImagePullPolicy;
    }

    const backupSpec: Record<string, unknown> = {};
    if (draft.backupEnabled) backupSpec.enabled = true;
    if (draft.backupSchedule.trim()) backupSpec.schedule = draft.backupSchedule.trim();
    const retentionDays = parseOptionalInt(draft.backupRetentionDays);
    if (retentionDays !== undefined) backupSpec.retentionDays = retentionDays;

    const resourceOverlay: ResourceOverlay = {};
    if (draft.resourceOverlay.statefulSet?.trim()) resourceOverlay.statefulSet = draft.resourceOverlay.statefulSet;
    if (draft.resourceOverlay.service?.trim()) resourceOverlay.service = draft.resourceOverlay.service;
    if (draft.resourceOverlay.ingress?.trim()) resourceOverlay.ingress = draft.resourceOverlay.ingress;

    let podOverrides: PodOverrides | undefined;
    if (draft.podOverridesText.trim()) {
      try {
        // YAML is a JSON superset, so this accepts both JSON and YAML input.
        podOverrides = parseYAML(draft.podOverridesText) as PodOverrides;
      } catch {
        // Surfaced as a field error in the Advanced options step; omitted here.
      }
    }

    const probes = buildProbesSpec(draft.probes);

    const body: Record<string, unknown> = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "Controller",
      metadata: {
        name: draft.name,
        namespace: draft.namespace,
      },
      spec: {
        ...(draft.version ? { version: draft.version } : {}),
        ...(draft.bundleMode === "existing" && draft.existingBundleName
          ? { composedBundleRef: { name: draft.existingBundleName, namespace: draft.existingBundleNamespace } }
          : {}),
        resources: {
          requests: {
            ...(draft.cpu ? { cpu: draft.cpu } : {}),
            ...(draft.memory ? { memory: draft.memory } : {}),
          },
        },
        persistence: {
          size: draft.storage,
        },
        ...(Object.keys(ingressSpec).length > 0 ? { ingressSpec } : {}),
        ...(draft.rbacGroups.length > 0
          ? { rbacSpec: { groups: draft.rbacGroups } }
          : {}),
        ...(draft.powerState ? { powerState: draft.powerState } : {}),
        ...(draft.className ? { className: draft.className } : {}),
        ...(Object.keys(reconciliationPolicy).length > 0 ? { reconciliationPolicy } : {}),
        ...(Object.keys(miteSpec).length > 0 ? { miteSpec } : {}),
        ...(Object.keys(backupSpec).length > 0 ? { backupSpec } : {}),
        ...(podOverrides ? { podOverrides } : {}),
        ...(probes ? { probes } : {}),
        ...(Object.keys(resourceOverlay).length > 0 ? { resourceOverlay } : {}),
      },
    };
    if (draft.bundleMode === "compose" && draft.composeSpec) {
      body["bundle"] = {
        ...draft.composeSpec,
        variables: draft.composeVariables,
      };
    }
    return body;
  };

  const handleDeploy = async () => {
    if (deploying || deployed) return;
    setDeploying(true);
    setDeployError(null);
    try {
      const body = buildSubmitBody();
      await createController(draft.cluster, draft.namespace, {
        metadata: { name: draft.name },
        spec: (body["spec"] as Record<string, unknown>) ?? {},
        ...(body["bundle"] ? { bundle: body["bundle"] as ComposedBundleSpec } : {}),
      });
      setDeployed(true);
      setTimelineIdx(0);

      const baseUrl = controllerEventsUrl(draft.cluster, draft.namespace, draft.name);
      // Mint a short-lived, scoped stream ticket instead of putting a session token
      // in the URL. EventSource cannot set an Authorization header.
      const { ticket } = await bffFetch<{ ticket: string; expiresInSeconds: number }>(
        "/stream/ticket",
        { method: "POST", body: JSON.stringify({ scope: `controller:${draft.cluster}/${draft.namespace}/${draft.name}` }) },
      );
      const sep = baseUrl.includes("?") ? "&" : "?";
      const es = new EventSource(`${baseUrl}${sep}ticket=${encodeURIComponent(ticket)}`);
      es.onmessage = (event) => {
        try {
          const data = JSON.parse(event.data);
          if (data.event === "provisioning.started") setTimelineIdx((prev) => Math.max(prev, 1));
          if (data.event === "provisioning.completed") setTimelineIdx((prev) => Math.max(prev, 2));
          if (data.event === "connected") setTimelineIdx((prev) => Math.max(prev, 3));
          if (data.event === "phase" && data.phase === "Connected") setTimelineIdx((prev) => Math.max(prev, 4));
          if (data.event === "provisioning.failed") {
            setDeployError(data.reason || data.message || "Provisioning failed");
            es.close();
          }
        } catch {
          // ignore parse errors
        }
      };
      es.onerror = () => {
        es.close();
      };
      setTimeout(() => es.close(), 120000);
    } catch (e: unknown) {
      setDeployError(e instanceof Error ? e.message : "Deploy failed");
    } finally {
      setDeploying(false);
    }
  };

  const handleDownloadYaml = async () => {
    try {
      const y = await renderController(draft.cluster, draft.namespace, buildSubmitBody());
      const blob = new Blob([y], { type: "application/yaml" });
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = `${draft.name}.yaml`;
      a.click();
    } catch {
      // ignore
    }
  };

  const resolveCatalogItem = (
    ref: { name: string; namespace?: string },
  ): CatalogItemSummary | undefined => {
    if (ref.namespace) {
      return catalogItems.find(
        (ci) => ci.name === ref.name && ci.namespace === ref.namespace,
      );
    }
    // Unset ref: mirror backend S1 order — draft's target namespace first, then operator namespace.
    const targetNs = draft.namespace;
    return (
      catalogItems.find(
        (ci) => ci.name === ref.name && ci.namespace === targetNs,
      ) ??
      (catalogOperatorNs
        ? catalogItems.find(
            (ci) => ci.name === ref.name && ci.namespace === catalogOperatorNs,
          )
        : undefined) ??
      catalogItems.find((ci) => ci.name === ref.name)
    );
  };

  // Resolves an unresolved ${var} name to the CatalogVariable that declares
  // it, by iterating draft.composeSpec.inputs in array order (the order the
  // user added inputs — stable/deterministic) and searching each resolved
  // item summary's variables for a name match. First match wins on duplicates;
  // this only decides which widget shape renders, never which value wins —
  // the wire-level rawVars precedence in composer.go is unaffected.
  const declaredVar = (name: string): CatalogVariable | undefined => {
    for (const input of draft.composeSpec?.inputs ?? []) {
      if (!input.itemRef) continue;
      const item = resolveCatalogItem(input.itemRef);
      const match = item?.variables?.find((v) => v.name === name);
      if (match) return match;
    }
    return undefined;
  };

  // Partition the (search-filtered) catalog items into ordered, labeled groups:
  // platform/shared first, then the draft's target namespace, then each remaining
  // namespace as its own group. Until the operator namespace is known (list not yet
  // loaded), fall back to a single ungrouped list to avoid a mislabeled group.
  const pickerGroups = (): { key: string; label: string; items: CatalogItemSummary[] }[] => {
    const filtered = catalogItems.filter(
      (ci) =>
        !catalogSearch ||
        ci.name.toLowerCase().includes(catalogSearch.toLowerCase()) ||
        (ci.displayName ?? "").toLowerCase().includes(catalogSearch.toLowerCase()),
    );
    if (!catalogOperatorNs) {
      return [{ key: "all", label: "", items: filtered }];
    }
    const targetNs = draft.namespace;
    const platform: CatalogItemSummary[] = [];
    const tenant: CatalogItemSummary[] = [];
    const rest = new Map<string, CatalogItemSummary[]>();
    for (const ci of filtered) {
      const ns = ci.namespace ?? "";
      if (ns === catalogOperatorNs) {
        platform.push(ci);
      } else if (targetNs && ns === targetNs) {
        tenant.push(ci);
      } else {
        const bucket = rest.get(ns) ?? [];
        bucket.push(ci);
        rest.set(ns, bucket);
      }
    }
    const groups: { key: string; label: string; items: CatalogItemSummary[] }[] = [];
    if (platform.length) {
      groups.push({ key: "__platform", label: `Platform / shared (${catalogOperatorNs})`, items: platform });
    }
    if (tenant.length) {
      groups.push({ key: "__tenant", label: `${targetNs} (this controller)`, items: tenant });
    }
    for (const ns of Array.from(rest.keys()).sort()) {
      groups.push({ key: ns, label: ns || "(no namespace)", items: rest.get(ns)! });
    }
    return groups;
  };

  const hasFail = preflightChecks.some((c) => c.status === "fail");

  if (configLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading configuration...</div>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <button className={styles.backLink} onClick={() => navigate("/controllers")}>
        &larr; Back to controllers
      </button>

      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>New controller</div>
          <div className={styles.pageDesc}>
            Provision a managed Jenkins controller — pick a configuration bundle from the catalog, size it, and deploy.
          </div>
        </div>
        <span className={styles.note}>
          Creates a <span className={styles.mono}>varroa.dev/v1alpha1 · Controller</span> resource
        </span>
      </div>

      <div className={styles.stepper}>
        {STEPS.map((s, i) => {
          const n = i + 1;
          const isOn = n === step;
          const isDone = n < step;
          const reachable = canReachStep(n);
          return (
            <div className={styles.stepTrack} key={n} style={{ flex: i < TOTAL_STEPS - 1 ? 1 : "none" }}>
              <button
                className={`${styles.step} ${isOn ? styles.stepOn : ""} ${isDone ? styles.stepDone : ""}`}
                onClick={() => goStep(n)}
                disabled={!reachable}
                aria-disabled={!reachable}
                title={reachable ? undefined : "Complete previous steps first"}
              >
                <span className={styles.hex}>
                  <svg viewBox="0 0 32 32">
                    <polygon points="16,2 28,9 28,23 16,30 4,23 4,9" />
                  </svg>
                  <span className={styles.hexIndex}>{isDone ? "\u2713" : n}</span>
                </span>
                <div>
                  <div className={styles.sLabel}>{s.label}</div>
                  <div className={styles.sSub}>{s.sub}</div>
                </div>
              </button>
              {i < TOTAL_STEPS - 1 && (
                <div className={`${styles.stepLine} ${isDone ? styles.stepLineDone : ""}`} />
              )}
            </div>
          );
        })}
      </div>

      <div className={styles.wizard}>
        <div className={styles.card}>
          {/* === STEP 1: BASICS === */}
          {step === 1 && (
            <div className={styles.pane}>
              <div className={styles.paneTitle}>Basics</div>
              <div className={styles.paneDesc}>
                Identity and runtime of the controller. Everything else can be changed later by editing the resource.
              </div>

              {clustersError || (clusters && clusters.length === 0) ? (
                <div className={styles.errorBanner}>
                  Cluster information is unavailable. The wizard cannot proceed without cluster data.
                </div>
              ) : null}

              {clusters && clusters.length >= 2 ? (
                <div className={styles.field}>
                  <label>Cluster</label>
                  <select
                    className={styles.select}
                    value={draft.cluster}
                    onChange={(e) => updateDraft({ cluster: e.target.value })}
                  >
                    {clusters
                      .filter((c) => c.healthy && c.state === "active")
                      .map((c) => (
                        <option key={c.name} value={c.name}>
                          {c.core ? `${c.name} (core)` : c.name}
                        </option>
                      ))}
                  </select>
                  <div className={styles.hint}>Target cluster for the controller.</div>
                </div>
              ) : null}

              <div className={styles.frow}>
                <div className={styles.field}>
                  <label>
                    Controller name <span className={styles.req}>*</span>
                  </label>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="team-web-builds"
                    value={draft.name}
                    onChange={(e) => {
                      const v = e.target.value.trim().toLowerCase();
                      updateDraft({ name: v });
                      debouncedNameCheck(v, draft.namespace);
                    }}
                    spellCheck={false}
                  />
                  <div className={styles.hint}>Lowercase DNS-1123. Becomes the StatefulSet + Service name.</div>
                  {nameCollision && (
                    <div className={styles.collisionWarn}>
                      A controller with this name already exists in {draft.namespace || "this namespace"}.
                    </div>
                  )}
                </div>
                <div className={styles.field}>
                  <label>
                    Namespace <span className={styles.req}>*</span>
                  </label>
                  {deployableNs?.degraded && (
                    <div className={styles.warnBanner}>
                      Cluster {draft.cluster} is unreachable — namespace suggestions are unavailable; deployment may fail until the cluster reconnects.
                    </div>
                  )}
                  {deployableNs?.allowFreeform ? (
                    <>
                      <input
                        className={`${styles.input} ${styles.inputMono}`}
                        placeholder="varroa-tenants"
                        value={draft.namespace}
                        onChange={(e) => {
                          updateDraft({ namespace: e.target.value.trim() });
                          if (draft.name) debouncedNameCheck(draft.name, e.target.value.trim());
                        }}
                        list="deployable-ns-suggestions"
                      />
                      {deployableNs.namespaces.length > 0 && (
                        <datalist id="deployable-ns-suggestions">
                          {deployableNs.namespaces.map((ns) => (
                            <option key={ns} value={ns} />
                          ))}
                        </datalist>
                      )}
                    </>
                  ) : (
                    <>
                      <select
                        className={styles.select}
                        value={draft.namespace}
                        onChange={(e) => {
                          updateDraft({ namespace: e.target.value });
                          if (draft.name) debouncedNameCheck(draft.name, e.target.value);
                        }}
                        disabled={!deployableNs || deployableNs.namespaces.length === 0}
                      >
                        {(deployableNs?.namespaces ?? []).length === 0 ? (
                          <option value="">—</option>
                        ) : (
                          (deployableNs?.namespaces ?? []).map((ns) => (
                            <option key={ns} value={ns}>
                              {ns}
                            </option>
                          ))
                        )}
                      </select>
                      {deployableNs && deployableNs.namespaces.length === 0 && (
                        <div className={`${styles.muted} ${styles.mutedNotice}`}>
                          You are not authorized to create controllers in any namespace.
                        </div>
                      )}
                    </>
                  )}
                </div>
              </div>

              <div className={styles.field}>
                <label>Jenkins version</label>
                <VersionPicker
                  versions={draft.config?.versions ?? []}
                  value={draft.version}
                  onChange={(v) => updateDraft({ version: v })}
                />
              </div>

              <div className={styles.field}>
                <label>
                  Description <span className={`${styles.muted} ${styles.medium}`}>(optional)</span>
                </label>
                <input
                  className={styles.input}
                  placeholder="CI for the web frontend team"
                  value={draft.description}
                  onChange={(e) => updateDraft({ description: e.target.value })}
                />
              </div>

              <div className={styles.paneFoot}>
                <span className={`${styles.muted} ${styles.stepCount}`}>
                  Step 1 of {TOTAL_STEPS}
                </span>
                <span className={styles.spacer} />
                <button className={`${styles.btn} ${styles.btnPrimary}`} disabled={!canProceed()} onClick={handleNext}>
                  Continue &rarr; Bundle
                </button>
              </div>
            </div>
          )}

          {/* === STEP 2: BUNDLE === */}
          {step === 2 && (
            <div className={styles.pane}>
              <div className={styles.paneTitle}>Configuration bundle</div>
              <div className={styles.paneDesc}>
                A <b>ComposedBundle</b> defines the controller&apos;s JCasC, plugins, pod templates, and RBAC.
              </div>

              <div className={styles.bundleModeRow}>
                <div className={styles.seg}>
                  <button
                    className={draft.bundleMode === "existing" ? styles.segBtnOn : ""}
                    onClick={() => updateDraft({ bundleMode: "existing", composeSpec: null })}
                  >
                    Use existing
                  </button>
                  <button
                    className={draft.bundleMode === "compose" ? styles.segBtnOn : ""}
                    onClick={() =>
                      updateDraft({
                        bundleMode: "compose",
                        existingBundleName: null,
                        existingBundleNamespace: "",
                        composeSpec: draft.composeSpec ?? { inputs: [] },
                      })
                    }
                  >
                    Compose new
                  </button>
                  <button
                    className={draft.bundleMode === "none" ? styles.segBtnOn : ""}
                    onClick={() => updateDraft({ bundleMode: "none", existingBundleName: null, existingBundleNamespace: "", composeSpec: null })}
                  >
                    None
                  </button>
                </div>
              </div>

              {draft.bundleMode === "existing" && (
                <div>
                  <div className={styles.field}>
                    <input
                      className={styles.input}
                      placeholder="Search bundles..."
                      value={bundleSearch}
                      onChange={(e) => setBundleSearch(e.target.value)}
                    />
                  </div>
                  <div className={styles.bundleGrid}>
                    {filteredBundles.map((b) => {
                      const hash = b.status?.resolvedHash ?? "";
                      const shortHash = hash ? hash.slice(0, 7) : "\u2014";
                      const summary = b.status?.inputSummary;
                      return (
                        <div
                          key={b.metadata.name}
                          className={`${styles.bundleCard} ${draft.existingBundleName === b.metadata.name ? styles.bundleCardOn : ""}`}
                          onClick={() => updateDraft({ existingBundleName: b.metadata.name, existingBundleNamespace: b.metadata.namespace })}
                        >
                          {draft.existingBundleName === b.metadata.name && <span className={styles.oCheck}>&#10003;</span>}
                          <div className={styles.bCardTop}>
                            <div className={styles.bCardIc}>&#9670;</div>
                            <div>
                              <div className={styles.bCardName}>{b.spec.displayName || b.metadata.name}</div>
                              <div className={styles.bCardNs}>{b.metadata.namespace}</div>
                            </div>
                          </div>
                          <div className={styles.bCardDesc}>{b.spec.description || ""}</div>
                          <div className={styles.bCardFoot}>
                            <span className={styles.hash}>
                              {b.status?.phase === "Ready" ? "Ready" : b.status?.phase ?? "Pending"}
                            </span>
                            <span className={styles.hash}>rev {shortHash}</span>
                            {summary && summary.length > 0 && (
                              <div className={styles.bCardInputs}>
                                {summary.slice(0, 4).map((s, i) => {
                                  const icon = TYPE_ICONS[s.type] ?? TYPE_ICONS.git;
                                  return (
                                    <i key={i} className={icon.cls}>
                                      {icon.label}
                                    </i>
                                  );
                                })}
                                {summary.length > 4 && (
                                  <span className={styles.bCardInputsMore}>+{summary.length - 4}</span>
                                )}
                              </div>
                            )}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                  {filteredBundles.length === 0 && (
                    <div className={`${styles.muted} ${styles.emptyState}`}>
                      No bundles found. Create one from the Catalog Browser first.
                    </div>
                  )}
                </div>
              )}

              {draft.bundleMode === "compose" && (
                <div>
                  <div className={styles.field}>
                    <label>Bundle inputs</label>
                    <div className={styles.composeList}>
                      {(draft.composeSpec?.inputs ?? []).map((input, idx) => {
                        if (input.itemRef) {
                          const item = resolveCatalogItem(input.itemRef);
                          const type = item?.type ?? "item";
                          const icon = TYPE_ICONS[type] ?? TYPE_ICONS.item;
                          return (
                            <div key={idx} className={styles.inputRow}>
                              <span className={styles.drag}>&#8942;&#8942;</span>
                              <span className={`${styles.tIc} ${icon.cls}`}>{icon.label}</span>
                              <div>
                                <div className={styles.iName}>
                                  {item?.displayName || input.itemRef.name}
                                  {input.itemRef.namespace && (
                                    <span className={styles.nsBadge}>{input.itemRef.namespace}</span>
                                  )}
                                </div>
                                <div className={styles.iSub}>
                                  {type} {item?.version ? `· ${item.version}` : ""}
                                </div>
                              </div>
                              <span className={styles.iKind}>catalog item</span>
                              <button className={styles.rm} onClick={() => removeComposeInput(idx)} title="Remove">
                                &#10005;
                              </button>
                            </div>
                          );
                        }
                        if (input.gitSource) {
                          return (
                            <div key={idx} className={styles.inputRow}>
                              <span className={styles.drag}>&#8942;&#8942;</span>
                              <span className={`${styles.tIc} ${TYPE_ICONS.git.cls}`}>{TYPE_ICONS.git.label}</span>
                              <div>
                                <div className={styles.iName}>{input.gitSource.repoURL}</div>
                                <div className={styles.iSub}>
                                  {input.gitSource.path} · {input.gitSource.revision ?? "main"}
                                </div>
                              </div>
                              <span className={styles.iKind}>git source</span>
                              <button className={styles.rm} onClick={() => removeComposeInput(idx)} title="Remove">
                                &#10005;
                              </button>
                            </div>
                          );
                        }
                        return null;
                      })}
                    </div>
                    <div className={styles.addRow}>
                      <button className={styles.dashedBtn} onClick={() => setShowCatalogPicker(!showCatalogPicker)}>
                        &#xFF0B; Add catalog item
                      </button>
                      <button className={styles.dashedBtn} onClick={() => setShowGitForm(!showGitForm)}>
                        &#x2387; Add git source
                      </button>
                    </div>
                    {showCatalogPicker && (
                      <div className={styles.catalogPicker}>
                        <input
                          placeholder="Search catalog..."
                          value={catalogSearch}
                          onChange={(e) => setCatalogSearch(e.target.value)}
                          className={`${styles.input} ${styles.pickerSearch}`}
                        />
                        {pickerGroups().map((group) => (
                          <div key={group.key} data-testid={`picker-group-${group.key}`}>
                            {group.label && (
                              <div
                                className={styles.pickerGroupLabel}
                              >
                                {group.label}
                              </div>
                            )}
                            {group.items.map((ci) => (
                              <div
                                key={`${ci.namespace}/${ci.name}`}
                                className={styles.pickerItem}
                                onClick={() => addComposeInput(ci)}
                              >
                                {ci.displayName || ci.name}
                                <span className={styles.nsBadge}>{ci.namespace}</span>{" "}
                                <span className={styles.pickerMeta}>
                                  {ci.type} · {ci.version ?? "latest"}
                                </span>
                              </div>
                            ))}
                          </div>
                        ))}
                      </div>
                    )}
                    {showGitForm && (
                      <div className={styles.gitForm}>
                        <div className={styles.field}>
                          <label>Repo URL</label>
                          <input className={`${styles.input} ${styles.inputMono}`} value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} placeholder="git@github.com:acme/team-jenkins" />
                        </div>
                        <div className={styles.gitFormRow}>
                          <div className={`${styles.field} ${styles.grow}`}>
                            <label>Path</label>
                            <input className={`${styles.input} ${styles.inputMono}`} value={gitPath} onChange={(e) => setGitPath(e.target.value)} placeholder="/" />
                          </div>
                          <div className={`${styles.field} ${styles.grow}`}>
                            <label>Revision (optional)</label>
                            <input className={`${styles.input} ${styles.inputMono}`} value={gitRevision} onChange={(e) => setGitRevision(e.target.value)} placeholder="main" />
                          </div>
                        </div>
                        <button className={`${styles.btn} ${styles.addButton}`} onClick={addGitInput}>
                          Add
                        </button>
                        <button className={`${styles.btn} ${styles.btnGhost}`} onClick={() => setShowGitForm(false)}>
                          Cancel
                        </button>
                      </div>
                    )}
                  </div>

                  <div className={styles.field}>
                    <label>
                      Bundle variables{" "}
                      <span className={`${styles.muted} ${styles.medium}`}>
                        — <span className={styles.mono}>varroa_*</span> injected automatically
                      </span>
                    </label>
                    <div className={styles.vars}>
                      {Object.entries(draft.composeVariables).map(([k, v], idx) => (
                        <div key={idx} className={styles.varRow}>
                          <input
                            value={k}
                            onChange={(e) => {
                              const entries = Object.entries(draft.composeVariables);
                              entries[idx] = [e.target.value, v];
                              updateDraft({ composeVariables: Object.fromEntries(entries) });
                            }}
                            placeholder="key"
                            spellCheck={false}
                          />
                          <input
                            value={v}
                            onChange={(e) => {
                              const entries = Object.entries(draft.composeVariables);
                              entries[idx] = [k, e.target.value];
                              updateDraft({ composeVariables: Object.fromEntries(entries) });
                            }}
                            placeholder="value"
                            spellCheck={false}
                          />
                          <button
                            className={styles.rm}
                            onClick={() => {
                              const entries = Object.entries(draft.composeVariables);
                              entries.splice(idx, 1);
                              updateDraft({ composeVariables: Object.fromEntries(entries) });
                            }}
                          >
                            &#10005;
                          </button>
                        </div>
                      ))}
                      <div className={styles.varRow}>
                        <input
                          placeholder="key"
                          spellCheck={false}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") {
                              const target = e.target as HTMLInputElement;
                              const val = (target.parentElement?.nextElementSibling as HTMLInputElement)?.value ?? "";
                              if (target.value.trim()) {
                                updateDraft({ composeVariables: { ...draft.composeVariables, [target.value.trim()]: val } });
                                target.value = "";
                              }
                            }
                          }}
                        />
                        <input placeholder="value" spellCheck={false} />
                        <button className={styles.rm}>&#10005;</button>
                      </div>
                    </div>
                  </div>

                  <div className={styles.previewAction}>
                    <button className={`${styles.btn} ${styles.btnSm}`} onClick={handlePreviewBundle}>
                      Preview merged JCasC
                    </button>
                  </div>

                  {previewError && <div className={styles.errorBanner}>{previewError}</div>}

                  {previewMissing.length > 0 && (
                    <div className={styles.errorBanner}>Missing items: {previewMissing.join(", ")}</div>
                  )}

                  {previewDrifted.length > 0 && (
                    <div className={styles.warnBanner}>Drifted items: {previewDrifted.join(", ")}</div>
                  )}

                  {previewWarnings.length > 0 && (
                    <div className={styles.warnBanner}>
                      {previewWarnings.map((w, i) => (
                        <div key={i}>{w}</div>
                      ))}
                    </div>
                  )}

                  {previewOutput && (
                    <div className={styles.previewOutput}>
                      <div className={styles.yaml}>{previewOutput}</div>
                    </div>
                  )}

                  {unresolvedVars.length > 0 && (
                    <div className={styles.field}>
                      <label>Unresolved variables — must be filled before proceeding</label>
                      <div className={styles.unresolvedList}>
                        {unresolvedVars.map((v) => {
                          const decl = declaredVar(v);
                          const effectiveType = decl?.type || "string";
                          const value = draft.composeVariables[v] ?? "";
                          const setValue = (val: string) =>
                            updateDraft({ composeVariables: { ...draft.composeVariables, [v]: val } });

                          if (decl && effectiveType === "boolean") {
                            return (
                              <label
                                key={v}
                                className={`${styles.unresolvedPill} ${styles.varPill}`}
                              >
                                <input
                                  type="checkbox"
                                  checked={value === "true"}
                                  onChange={(e) => setValue(e.target.checked ? "true" : "false")}
                                />
                                {v}
                              </label>
                            );
                          }

                          if (decl && effectiveType === "credentials") {
                            return (
                              <span
                                key={v}
                                className={`${styles.unresolvedPill} ${styles.varPill}`}
                              >
                                {v}
                                <input
                                  className={`${styles.input} ${styles.varInput160}`}
                                  value={value}
                                  onChange={(e) => setValue(e.target.value)}
                                  placeholder="Jenkins credentials ID"
                                />
                              </span>
                            );
                          }

                          if (
                            decl &&
                            decl.allowedValues &&
                            decl.allowedValues.length > 0 &&
                            (effectiveType === "string" || effectiveType === "number")
                          ) {
                            return (
                              <span
                                key={v}
                                className={`${styles.unresolvedPill} ${styles.varPill}`}
                              >
                                {v}
                                <select
                                  className={`${styles.input} ${styles.varInput140}`}
                                  value={value}
                                  onChange={(e) => setValue(e.target.value)}
                                >
                                  <option value="" disabled>
                                    select…
                                  </option>
                                  {decl.allowedValues.map((av) => (
                                    <option key={av} value={av}>
                                      {av}
                                    </option>
                                  ))}
                                </select>
                              </span>
                            );
                          }

                          if (decl && effectiveType === "number") {
                            return (
                              <span
                                key={v}
                                className={`${styles.unresolvedPill} ${styles.varPill}`}
                              >
                                {v}
                                <input
                                  type="number"
                                  className={`${styles.input} ${styles.varInput100}`}
                                  value={value}
                                  onChange={(e) => setValue(e.target.value)}
                                />
                              </span>
                            );
                          }

                          // No declared match, or a declared plain string with no
                          // allowedValues — existing raw key/value fallback, unchanged.
                          return (
                            <span
                              key={v}
                              className={`${styles.unresolvedPill} ${styles.varPill} ${styles.varPillClickable}`}
                              onClick={() => {
                                if (!draft.composeVariables[v]) {
                                  updateDraft({ composeVariables: { ...draft.composeVariables, [v]: "" } });
                                }
                              }}
                            >
                              {v} &#x2795;
                            </span>
                          );
                        })}
                      </div>
                    </div>
                  )}

                  <div className={styles.mergeNote}>
                    Saves as ComposedBundle <span className={styles.mono}>{draft.name || "controller"}-bundle</span> in{" "}
                    <span className={styles.mono}>{draft.namespace || "default"}</span>
                  </div>
                </div>
              )}

              {draft.bundleMode === "none" && (
                <div className={`${styles.muted} ${styles.emptyState}`}>
                  No bundle will be referenced. The controller will start with defaults only.
                </div>
              )}

              <div className={styles.paneFoot}>
                <button className={`${styles.btn} ${styles.btnGhost}`} onClick={handleBack}>
                  &larr; Back
                </button>
                <span className={`${styles.muted} ${styles.stepCount}`}>
                  Step 2 of {TOTAL_STEPS}
                </span>
                <span className={styles.spacer} />
                <button className={`${styles.btn} ${styles.btnPrimary}`} disabled={!canProceed()} onClick={handleNext}>
                  Continue &rarr; Resources
                </button>
              </div>
            </div>
          )}

          {/* === STEP 3: RESOURCES & ACCESS === */}
          {step === 3 && (
            <div className={styles.pane}>
              <div className={styles.paneTitle}>Resources &amp; access</div>
              <div className={styles.paneDesc}>
                Compute footprint, who can reach the controller, and where it&apos;s exposed.
              </div>

              {draft.config?.sizePresets && draft.config.sizePresets.length > 0 && (
                <div className={styles.field}>
                  <label>Controller size</label>
                  <div className={styles.optGrid}>
                    {draft.config.sizePresets.map((sp) => {
                      const selected =
                        draft.cpu === sp.cpu && draft.memory === sp.memory && draft.storage === sp.storage;
                      return (
                        <div
                          key={sp.name}
                          className={`${styles.opt} ${selected ? styles.optOn : ""}`}
                          onClick={() =>
                            updateDraft({ cpu: sp.cpu, memory: sp.memory, storage: sp.storage })
                          }
                        >
                          <span className={styles.oCheck}>&#10003;</span>
                          <div className={styles.oName}>{sp.name}</div>
                          <div className={`${styles.oSub} ${styles.mono}`}>
                            {sp.cpu} cpu · {sp.memory} · {sp.storage}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </div>
              )}

              <div className={styles.frow3}>
                <div className={styles.field}>
                  <label>CPU</label>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="2"
                    value={draft.cpu}
                    onChange={(e) => updateDraft({ cpu: e.target.value })}
                  />
                </div>
                <div className={styles.field}>
                  <label>Memory</label>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="4Gi"
                    value={draft.memory}
                    onChange={(e) => updateDraft({ memory: e.target.value })}
                  />
                </div>
                <div className={styles.field}>
                  <label>Storage</label>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="20Gi"
                    value={draft.storage}
                    onChange={(e) => updateDraft({ storage: e.target.value })}
                  />
                </div>
              </div>

              <div className={styles.field}>
                <label>Routing mode</label>
                <div className={styles.optGrid}>
                  {[
                    { value: "" as const, label: "Subdomain (default)", sub: "name.rootDomain" },
                    { value: "path" as const, label: "Path-based", sub: "shared host, /jenkins/<ns>/<name>" },
                  ].map((opt) => (
                    <div
                      key={opt.value || "subdomain"}
                      className={`${styles.opt} ${draft.ingressMode === opt.value ? styles.optOn : ""}`}
                      onClick={() => updateDraft({ ingressMode: opt.value })}
                    >
                      <span className={styles.oCheck}>&#10003;</span>
                      <div className={styles.oName}>{opt.label}</div>
                      <div className={`${styles.oSub} ${styles.mono}`}>{opt.sub}</div>
                    </div>
                  ))}
                </div>
              </div>

              <div className={styles.field}>
                <label>Ingress host</label>
                <input
                  className={`${styles.input} ${styles.inputMono}`}
                  placeholder={derivedHost || "auto-derived"}
                  value={draft.ingressHost}
                  onChange={(e) => updateDraft({ ingressHost: e.target.value.trim() })}
                />
                {draft.ingressMode === "path" ? (
                  <div className={styles.hint}>
                    Path mode requires the shared brood dashboard host — check with your cluster
                    admin if you&apos;re unsure. The path prefix{" "}
                    <span className={styles.hash}>/jenkins/{draft.namespace || "ns"}/{draft.name || "name"}</span>{" "}
                    is derived automatically.
                  </div>
                ) : (
                  <div className={styles.hint}>
                    Leave blank to derive{" "}
                    <span className={styles.hash}>{draft.name || "name"}.{draft.config?.rootDomain || "..."}</span>{" "}
                    from brood root domain
                  </div>
                )}
              </div>

              <div className={styles.field}>
                <label>TLS secret name (optional)</label>
                <input
                  className={`${styles.input} ${styles.inputMono}`}
                  placeholder="e.g. jenkins-tls"
                  value={draft.ingressTlsSecretName}
                  onChange={(e) => updateDraft({ ingressTlsSecretName: e.target.value })}
                />
              </div>

              <div className={styles.field}>
                <label>RBAC groups</label>
                {draft.rbacGroups.map((g, idx) => {
                  const groupInfo = groups.find((gr) => gr.name === g.name);
                  const memberCount = groupInfo?.memberCount;
                  return (
                    <div key={idx} className={styles.grpRow}>
                      <span className={styles.gIc}>&#x2687;</span>
                      <b>{g.name}</b>
                      {memberCount !== undefined && memberCount === 0 && (
                        <span className={styles.warnInline} title="No members seen yet">
                          &#x26A0; no members seen yet
                        </span>
                      )}
                      <select
                        className={styles.grpSelect}
                        value={g.role}
                        onChange={(e) => updateRbacRole(idx, e.target.value)}
                      >
                        {BUILTIN_ROLES.map((r) => (
                          <option key={r} value={r}>
                            {r}
                          </option>
                        ))}
                      </select>
                      <button className={styles.rm} onClick={() => removeRbacGroup(idx)} title="Remove">
                        &#10005;
                      </button>
                    </div>
                  );
                })}
                <div className={styles.addRow}>
                  <div className={`${styles.typeaheadWrap} ${styles.typeaheadGrow}`}>
                    <input
                      className={`${styles.input} ${styles.dashedBtn} ${styles.typeaheadButton}`}
                      placeholder="+ Add group (type to search)"
                      value={groupTypeahead}
                      onChange={(e) => {
                        setGroupTypeahead(e.target.value);
                        setShowGroupDropdown(true);
                      }}
                      onFocus={() => setShowGroupDropdown(true)}
                      onBlur={() => setTimeout(() => setShowGroupDropdown(false), 200)}
                    />
                    {showGroupDropdown && filteredGroups.length > 0 && (
                      <div className={styles.typeaheadOptions}>
                        {filteredGroups
                          .filter((g) => !draft.rbacGroups.some((rg) => rg.name === g.name))
                          .slice(0, 10)
                          .map((g) => (
                            <div
                              key={g.name}
                              className={styles.typeaheadOption}
                              onMouseDown={() => addRbacGroup(g.name)}
                            >
                              <span>&#x2687;</span>
                              <span>{g.name}</span>
                              <span className={styles.memberCount}>
                                {g.memberCount ?? "?"} members
                              </span>
                            </div>
                          ))}
                      </div>
                    )}
                  </div>
                </div>
                <div className={`${styles.hint} ${styles.mt8}`}>
                  <span className={styles.mono}>varroa:system-mite</span> and built-in roles are attached automatically.
                </div>
              </div>

              <div className={styles.paneFoot}>
                <button className={`${styles.btn} ${styles.btnGhost}`} onClick={handleBack}>
                  &larr; Back
                </button>
                <span className={`${styles.muted} ${styles.stepCount}`}>
                  Step 3 of {TOTAL_STEPS}
                </span>
                <span className={styles.spacer} />
                <button className={`${styles.btn} ${styles.btnPrimary}`} disabled={!canProceed()} onClick={handleNext}>
                  Continue &rarr; Advanced options
                </button>
              </div>
            </div>
          )}

          {/* === STEP 4: ADVANCED OPTIONS === */}
          {step === 4 && (
            <div className={styles.pane}>
              <div className={styles.paneTitle}>Advanced options</div>
              <div className={styles.paneDesc}>
                Everything else in the controller spec. All optional — leave blank to use cluster
                defaults.
              </div>

              <div className={styles.field}>
                <label>Ingress class name (optional)</label>
                <input
                  className={`${styles.input} ${styles.inputMono}`}
                  placeholder="cluster default"
                  value={draft.ingressClassName}
                  onChange={(e) => updateDraft({ ingressClassName: e.target.value })}
                />
              </div>

              <div className={styles.field}>
                <label>Ingress annotations</label>
                {Object.entries(draft.ingressAnnotations).map(([k, v]) => (
                  <div key={k} className={styles.grpRow}>
                    <span className={styles.mono}>{k}</span>
                    <span className={styles.muted}>=</span>
                    <span className={styles.mono}>{v}</span>
                    <button className={styles.rm} onClick={() => removeIngressAnnotation(k)} title="Remove">
                      &#10005;
                    </button>
                  </div>
                ))}
                <div className={styles.addRow}>
                  <input
                    className={`${styles.input} ${styles.inputMono} ${styles.grow}`}
                    placeholder="annotation key"
                    value={annotationKeyDraft}
                    onChange={(e) => setAnnotationKeyDraft(e.target.value)}
                  />
                  <input
                    className={`${styles.input} ${styles.inputMono} ${styles.grow}`}
                    placeholder="value"
                    value={annotationValueDraft}
                    onChange={(e) => setAnnotationValueDraft(e.target.value)}
                  />
                  <button className={`${styles.btn} ${styles.btnGhost}`} onClick={addIngressAnnotation}>
                    + Add
                  </button>
                </div>
                <div className={styles.hint}>
                  Merged over cluster-wide ingress annotation defaults; this controller&apos;s value
                  wins on key conflict.
                </div>
              </div>

              <div className={styles.field}>
                <label>Power state</label>
                <select
                  className={styles.input}
                  value={draft.powerState}
                  onChange={(e) => updateDraft({ powerState: e.target.value as WizardDraft["powerState"] })}
                >
                  <option value="">Running (default)</option>
                  <option value="Stopped">Stopped</option>
                  <option value="Hibernated">Hibernated</option>
                </select>
              </div>

              <div className={styles.field}>
                <label>Reconciliation policy</label>
                <div className={styles.frow3}>
                  <select
                    className={styles.input}
                    value={draft.reconciliationMode}
                    onChange={(e) =>
                      updateDraft({ reconciliationMode: e.target.value as WizardDraft["reconciliationMode"] })
                    }
                  >
                    <option value="">Mode: cluster default</option>
                    <option value="automatic">automatic</option>
                    <option value="manual">manual</option>
                    <option value="idle">idle</option>
                  </select>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="interval, e.g. 30s"
                    value={draft.reconciliationInterval}
                    onChange={(e) => updateDraft({ reconciliationInterval: e.target.value })}
                  />
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="rolloutWave"
                    value={draft.reconciliationRolloutWave}
                    onChange={(e) => updateDraft({ reconciliationRolloutWave: e.target.value })}
                  />
                </div>
                <div className={`${styles.frow3} ${styles.mt8}`}>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="maxDeferSeconds"
                    value={draft.reconciliationMaxDeferSeconds}
                    onChange={(e) => updateDraft({ reconciliationMaxDeferSeconds: e.target.value })}
                  />
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="drainTimeoutSeconds"
                    value={draft.reconciliationDrainTimeoutSeconds}
                    onChange={(e) => updateDraft({ reconciliationDrainTimeoutSeconds: e.target.value })}
                  />
                </div>
                {!isValidOptionalInteger(draft.reconciliationRolloutWave) ||
                !isValidOptionalInteger(draft.reconciliationMaxDeferSeconds) ||
                !isValidOptionalInteger(draft.reconciliationDrainTimeoutSeconds) ? (
                  <div className={styles.inlineError}>
                    rolloutWave, maxDeferSeconds, and drainTimeoutSeconds must be whole numbers
                  </div>
                ) : null}
              </div>

              <div className={styles.field}>
                <label>Controller class</label>
                <input
                  className={`${styles.input} ${styles.inputMono}`}
                  placeholder="class name"
                  value={draft.className}
                  onChange={(e) => updateDraft({ className: e.target.value })}
                />
              </div>

              <div className={styles.field}>
                <label>Mite sidecar (optional)</label>
                <div className={styles.frow}>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="image (optional)"
                    value={draft.miteImage}
                    onChange={(e) => updateDraft({ miteImage: e.target.value })}
                  />
                  <select
                    className={styles.select}
                    value={draft.miteImagePullPolicy}
                    onChange={(e) => updateDraft({ miteImagePullPolicy: e.target.value })}
                  >
                    <option value="">Unset (default)</option>
                    <option value="Always">Always</option>
                    <option value="Never">Never</option>
                    <option value="IfNotPresent">IfNotPresent</option>
                  </select>
                </div>
                <div className={styles.frow}>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="cpu"
                    value={draft.miteCpu}
                    onChange={(e) => updateDraft({ miteCpu: e.target.value })}
                  />
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="memory"
                    value={draft.miteMemory}
                    onChange={(e) => updateDraft({ miteMemory: e.target.value })}
                  />
                </div>
              </div>

              <div className={styles.field}>
                <label>Backup</label>
                <div className={styles.grpRow}>
                  <input
                    type="checkbox"
                    aria-label="Enable backup"
                    checked={draft.backupEnabled}
                    onChange={(e) => updateDraft({ backupEnabled: e.target.checked })}
                  />
                  <span>Enabled</span>
                </div>
                <div className={`${styles.frow3} ${styles.mt8}`}>
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="schedule, e.g. 0 2 * * *"
                    value={draft.backupSchedule}
                    onChange={(e) => updateDraft({ backupSchedule: e.target.value })}
                  />
                  <input
                    className={`${styles.input} ${styles.inputMono}`}
                    placeholder="retentionDays"
                    value={draft.backupRetentionDays}
                    onChange={(e) => updateDraft({ backupRetentionDays: e.target.value })}
                  />
                </div>
                {!isValidOptionalInteger(draft.backupRetentionDays) ? (
                  <div className={styles.inlineError}>retentionDays must be a whole number</div>
                ) : null}
              </div>

              <details open className={styles.field}>
                <summary className={styles.detailsSummary}>Health probes</summary>
                <div className={`${styles.muted} ${styles.probeDescription}`}>
                  Jenkins container probes. Leave blank to keep the backend defaults.
                </div>
                {PROBE_KINDS.map((kind) => (
                  <ProbePanel
                    key={kind}
                    kind={kind}
                    form={draft.probes[kind]}
                    onChange={(next) => updateDraft({ probes: { ...draft.probes, [kind]: next } })}
                  />
                ))}
              </details>

              <div className={styles.field}>
                <label>Resource overrides (optional)</label>
                <OverlayEditor
                  values={{
                    statefulSet: draft.resourceOverlay.statefulSet ?? "",
                    service: draft.resourceOverlay.service ?? "",
                    ingress: draft.resourceOverlay.ingress ?? "",
                  }}
                  onChange={(key: OverlayResourceKind, value: string) =>
                    updateDraft({ resourceOverlay: { ...draft.resourceOverlay, [key]: value } })
                  }
                  podOverridesText={draft.podOverridesText}
                  onPodOverridesChange={(value: string) => updateDraft({ podOverridesText: value })}
                  fieldError={
                    overlayFieldError ??
                    (!isValidOptionalYAML(draft.podOverridesText)
                      ? { field: "podOverrides", message: "invalid YAML" }
                      : null)
                  }
                  warnings={overlayPreview?.warnings}
                />
                <div className={styles.mt12}>
                  <button
                    className={`${styles.btn} ${styles.btnGhost}`}
                    disabled={overlayPreviewing}
                    onClick={handlePreviewOverlay}
                  >
                    {overlayPreviewing ? "Previewing…" : "Preview merge"}
                  </button>
                </div>
                {overlayPreviewError && <div className={styles.hint}>{overlayPreviewError}</div>}
                {overlayPreview && (
                  <div className={styles.mt12}>
                    {OVERLAY_RESOURCES.map(({ key, label }) =>
                      overlayPreview.diff[key] !== undefined ? (
                        <div key={key} className={styles.mb12}>
                          <div className={styles.hint}>{label}</div>
                          <DiffView diff={overlayPreview.diff[key] ?? ""} />
                        </div>
                      ) : null,
                    )}
                  </div>
                )}
              </div>

              <div className={styles.paneFoot}>
                <button className={`${styles.btn} ${styles.btnGhost}`} onClick={handleBack}>
                  &larr; Back
                </button>
                <span className={`${styles.muted} ${styles.stepCount}`}>
                  Step 4 of {TOTAL_STEPS}
                </span>
                <span className={styles.spacer} />
                <button className={`${styles.btn} ${styles.btnPrimary}`} disabled={!canProceed()} onClick={handleNext}>
                  Continue &rarr; Review
                </button>
              </div>
            </div>
          )}

          {/* === STEP 5: REVIEW & DEPLOY === */}
          {step === 5 && (
            <div className={styles.pane}>
              <div className={styles.paneTitle}>Review &amp; deploy</div>
              <div className={styles.paneDesc}>
                Pre-flight checks against the live cluster. This is exactly what will be applied.
              </div>

              {deployed && (
                <div className={styles.deployedBanner}>
                  <span className={`${styles.pulse} ${timelineIdx >= 0 ? styles.pulseOn : ""}`}>
                    <i />
                  </span>
                  <div>
                    <div className={styles.strong}>
                      Controller <span className={styles.mono}>{draft.name}</span> created
                    </div>
                    <div className={`${styles.muted} ${styles.deployedDescription}`}>
                      Watching provisioning — follow live progress on the controller page.
                    </div>
                  </div>
                  <button
                    className={`${styles.backLink} ${styles.deployedLink}`}
                    onClick={() => navigate(controllerRoute(draft.cluster, draft.namespace, draft.name))}
                  >
                    Open controller &rarr;
                  </button>
                </div>
              )}

              {deployError && <div className={styles.errorBanner}>{deployError}</div>}

              <div className={styles.reviewKv}>
                <div className="row">
                  <div className="k">Controller</div>
                  <div className={`${styles.mono} v`}>{draft.name}</div>
                </div>
                <div className="row">
                  <div className="k">Namespace</div>
                  <div className={`${styles.mono} v`}>{draft.namespace}</div>
                </div>
                <div className="row">
                  <div className="k">Jenkins</div>
                  <div className={`${styles.mono} v`}>{draft.version || draft.config?.defaultVersion || "default"}</div>
                </div>
                <div className="row">
                  <div className="k">Bundle</div>
                  <div className={`${styles.mono} v`}>
                    {draft.bundleMode === "existing"
                      ? `${draft.existingBundleName}`
                      : draft.bundleMode === "compose"
                        ? `${draft.name || "controller"}-bundle (new)`
                        : "none"}
                  </div>
                </div>
                <div className="row">
                  <div className="k">Resources</div>
                  <div className={`${styles.mono} v`}>
                    {draft.cpu} cpu · {draft.memory} · {draft.storage}
                  </div>
                </div>
                <div className="row">
                  <div className="k">Ingress</div>
                  <div className={`${styles.mono} v`}>{derivedHost || "none"}</div>
                </div>
              </div>

              {preflightLoading && (
                <div className={`${styles.muted} ${styles.preflightStatus}`}>
                  Running preflight checks...
                </div>
              )}

              {!preflightLoading && preflightChecks.length > 0 && (
                <div className={styles.checkList}>
                  {preflightChecks.map((c) => (
                    <div key={c.id} className={styles.checkItem}>
                      <span
                        className={`${styles.ck} ${c.status === "pass" ? styles.ckOk : c.status === "warn" ? styles.ckWarn : styles.ckFail}`}
                      >
                        {c.status === "pass" ? "\u2713" : c.status === "warn" ? "!" : "\u2717"}
                      </span>
                      <span>
                        {c.id}: {c.message}
                      </span>
                    </div>
                  ))}
                </div>
              )}

              <div className={`${styles.paneFoot} ${styles.paneFootFlush}`}>
                <button className={`${styles.btn} ${styles.btnGhost}`} onClick={handleBack}>
                  &larr; Back
                </button>
                <span className={styles.spacer} />
                <button className={styles.btn} onClick={handleDownloadYaml}>
                  &#x2913; Download YAML
                </button>
                <button
                  className={`${styles.btn} ${styles.btnPrimary} ${styles.deployButton}`}
                  disabled={deploying || deployed || hasFail}
                  onClick={handleDeploy}
                >
                  {deploying ? "Deploying..." : deployed ? "Deployed" : "Deploy controller"}
                </button>
              </div>
            </div>
          )}
        </div>

        {/* Right rail */}
        <div className={styles.rail}>
          {step === 5 && (
            <div className={`${styles.card} ${styles.cardFlush}`}>
              <div className={styles.cardHead}>
                <div className={styles.cardTitle}>Timeline</div>
              </div>
              <div className={`${styles.cardBody} ${styles.flow}`}>
                {TIMELINE_STEPS.map((ts, i) => {
                  const isDone = i < timelineIdx;
                  const isActive = i === timelineIdx;
                  return (
                    <div
                      key={i}
                      className={`${styles.flowStep} ${isDone ? styles.flowDone : ""} ${isActive ? styles.flowActive : ""}`}
                    >
                      <span className={styles.flowIc}>
                        {isDone ? "\u2713" : isActive ? "\u25C7" : ["\u25C7", "]", "\u25C7", "\u2713"][i]}
                      </span>
                      <div>
                        <div className={styles.fName}>{ts.label}</div>
                        <div className={styles.fSub}>{ts.sub}</div>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
