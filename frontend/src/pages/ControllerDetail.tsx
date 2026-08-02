import { useState, useEffect, useCallback, useRef } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useController } from "../hooks/useControllers";
import { useClusters, coreOf } from "../hooks/useClusters";
import { clusterQuery, controllerRoute } from "../routing";
import { useEventStream } from "../hooks/useEventStream";
import { ApiError, bffFetch } from "../hooks/useApi";
import { ForbiddenPage, GenericErrorPage, NotFoundPage } from "../components/RecoveryState";
import { useComposedBundles } from "../hooks/useCatalog";
import { useToast } from "../components/Toast";
import { BFF_BASE, updateController, ControllerConflictError, approveRestart, reprovisionController, restartController, setPowerState, approveDeletion, getProvisioningConfig, getVersionProfiles, parsePreflightChecks } from "../api/client";
import VersionPicker from "../components/VersionPicker";
import { overlayJenkinsImage } from "../lib/overlay";
import { upgradeInfo, versionsDiffer } from "../lib/versionCatalog";
import { pluginDiff } from "../lib/pluginDiff";
import { StatusPill } from "../components/StatusPill";
import { Pulse } from "../components/Pulse";
import { Card } from "../components/Card";
import { Tabs } from "../components/Tabs";
import ActivityTimeline from "../components/ActivityTimeline";
import { age } from "../components/activityTimeline.util";
import { KVGrid } from "../components/KVGrid";
import { Console } from "../components/Console";
import { Button } from "../components/Button";
import { BundleSelector, BundleHealthBadge } from "../components/BundleSelector";
import { ObservabilityPanel } from "../components/ObservabilityPanel";
import PluginsTab from "./PluginsTab";
import { useAuth } from "../context/AuthContext";
import { canDoInNamespace } from "../hooks/usePermissions";
import type {
  ReconciliationPolicy,
  ReconciliationMode,
  PreflightCheck,
  ProbeSpec,
  ProbesSpec,
} from "../types";
import { PROBE_DEFAULTS } from "../types";
import { ConfigPipeline } from "../components/ConfigPipeline";
import { useControllerDiff } from "../hooks/useControllerDiff";
import ConflictDialog from "../components/ConflictDialog";
import SpecEditorCard from "../components/specform/SpecEditorCard";
import styles from "./ControllerDetail.module.css";

const TABS = [
  { id: "overview", label: <>▦ Overview</> },
  { id: "configuration", label: <>⚯ Configuration</> },
  { id: "observability", label: <>⌁ Observability</> },
  { id: "plugins", label: <>⌘ Plugins</> },
  { id: "diagnostics", label: <>◷ Diagnostics</> },
];

type ControllerData = NonNullable<ReturnType<typeof useController>["data"]>;

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
      <div className={styles.frow3} style={{ marginTop: 8 }}>
        <div>
          <label htmlFor={`probe-${kind}-initialDelaySeconds`}>Initial delay</label>
          <input
            id={`probe-${kind}-initialDelaySeconds`}
            type="number"
            placeholder={String(defaults.initialDelaySeconds)}
            value={form.initialDelaySeconds}
            onChange={(e) => update({ initialDelaySeconds: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
            aria-label={`${label} initial delay seconds`}
          />
        </div>
        <div>
          <label htmlFor={`probe-${kind}-periodSeconds`}>Period</label>
          <input
            id={`probe-${kind}-periodSeconds`}
            type="number"
            placeholder={String(defaults.periodSeconds)}
            value={form.periodSeconds}
            onChange={(e) => update({ periodSeconds: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
            aria-label={`${label} period seconds`}
          />
        </div>
        <div>
          <label htmlFor={`probe-${kind}-timeoutSeconds`}>Timeout</label>
          <input
            id={`probe-${kind}-timeoutSeconds`}
            type="number"
            placeholder={String(defaults.timeoutSeconds)}
            value={form.timeoutSeconds}
            onChange={(e) => update({ timeoutSeconds: e.target.value })}
            disabled={!form.enabled}
            className={styles.input}
            aria-label={`${label} timeout seconds`}
          />
        </div>
      </div>
      <div className={styles.frow3} style={{ marginTop: 8 }}>
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

/* ── Module-level buildStages (5.1) ── */

function buildStages(ctrl: ControllerData) {
  return [
    { label: "SOURCE" as const, status: "—", hash: "", timestamp: "" },
    { label: "COMPOSE" as const, status: ctrl.appliedBundleHash ? "✓ resolved" : "…", hash: ctrl.appliedBundleHash || "", timestamp: "" },
    {
      label: "DESIRE" as const,
      status: (() => {
        const converged = ctrl.lastApplyResult?.hash === ctrl.desiredStateHash && ctrl.lastApplyResult?.succeeded === true;
        const pending = !!ctrl.desiredStateHash && !!ctrl.lastApplyResult?.hash && ctrl.lastApplyResult.hash !== ctrl.desiredStateHash;
        const blocked = ctrl.rollout?.blocked;
        const paused = ctrl.rollout?.paused;
        if (converged) return "✓ converged";
        if (paused) return "⏸ paused";
        if (blocked) return "🚫 blocked";
        if (pending) return "⟳ pending";
        return "○ idle";
      })(),
      hash: ctrl.desiredStateHash || "",
      timestamp: "",
    },
    {
      label: "DELIVER" as const,
      status: ctrl.lastApplyResult ? (ctrl.lastApplyResult.succeeded ? "✓ applied" : "✗ failed") : "…",
      hash: ctrl.lastApplyResult?.hash || "",
      timestamp: ctrl.lastApplyResult?.timestamp || "",
      error: ctrl.lastApplyResult && !ctrl.lastApplyResult.succeeded
        ? (ctrl.lastApplyResult.sections?.filter((s) => !s.ok).map((s) => s.name).join(", ") + " failed" || undefined)
        : undefined,
      telemetry: ctrl.jenkinsVersion ? [`Jenkins ${ctrl.jenkinsVersion}`] : undefined,
    },
    {
      label: "LIVE" as const,
      status: ctrl.liveDrift?.detected ? "⚠ drift" : ctrl.miteConnected ? "✓ connected" : "✗ offline",
      hash: ctrl.liveDrift?.liveConfigHash || "",
      timestamp: "",
      telemetry: ctrl.miteConnected
        ? [
            ...(ctrl.miteVersion ? [`Mite ${ctrl.miteVersion}`] : []),
            ...(ctrl.lastSeen ? [`last seen ${new Date(ctrl.lastSeen).toLocaleTimeString()}`] : []),
            ...(ctrl.certExpiry ? [`cert expires ${new Date(ctrl.certExpiry).toLocaleDateString()}`] : []),
          ].filter(Boolean) as string[]
        : undefined,
    },
  ];
}

export default function ControllerDetail() {
  const { cluster = "", namespace = "", name = "" } = useParams();
  const { data: ctrl, isLoading, error } = useController(cluster, namespace, name);
  const { data: clusters } = useClusters();
  const core = coreOf(clusters);
  const isCore = core != null && cluster === core.name;
  const [activeTab, setActiveTab] = useState("overview");
  const { permissions } = useAuth();
  const { toast } = useToast();
  const queryClient = useQueryClient();

  /* ── Root lifts (5.1) ── */

  const canManage = canDoInNamespace(permissions, namespace, "controllers", "manage");
  const canUpdate = canDoInNamespace(permissions, namespace, "controllers", "update");

  // Namespace-qualified via effectiveBundle for the same reason as the bundle
  // card below: a cross-namespace ref or the operator-namespace starter is not
  // findable in the controller's own namespace.
  const { data: bundlesData } = useComposedBundles(
    cluster,
    ctrl?.effectiveBundle?.namespace ?? ctrl?.composedBundleRef?.namespace ?? namespace
  );
  const attachedBundle = bundlesData?.items?.find(
    (i) => i.metadata.name === (ctrl?.effectiveBundle?.name ?? ctrl?.composedBundleRef?.name)
  );

  const invalidateAll = useCallback(() => {
    queryClient.invalidateQueries({ queryKey: ["controller", cluster, namespace, name] });
    queryClient.invalidateQueries({ queryKey: ["controllers"] });
  }, [queryClient, cluster, namespace, name]);

  const reprovision = useMutation({
    mutationFn: () => reprovisionController(cluster, name, namespace),
    onSuccess: () => { toast("Reprovision triggered"); invalidateAll(); },
    onError: (e) => toast(`Reprovision failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const restart = useMutation({
    mutationFn: () => restartController(cluster, name, namespace),
    onSuccess: () => { toast("Restart triggered"); invalidateAll(); },
    onError: (e) => toast(`Restart failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const power = useMutation({
    mutationFn: (state: "Running" | "Stopped" | "Hibernated") => setPowerState(cluster, name, namespace, state),
    onSuccess: () => { toast("Power state updated"); invalidateAll(); },
    onError: (e) => toast(`Power state change failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const reloadConfig = useMutation({
    mutationFn: () => approveRestart(cluster, namespace, name, "reload"),
    onSuccess: () => { toast("Reload triggered"); invalidateAll(); },
    onError: (e) => toast(`Reload failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const detachMutation = useMutation({
    mutationFn: () => updateController(cluster, name, namespace, { spec: { composedBundleRef: null } }),
    onSuccess: () => { toast("Bundle detached"); invalidateAll(); },
    onError: (e) => toast(`Detach failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const [reconcilePending, setReconcilePending] = useState(false);
  const reconcile = useCallback(async () => {
    setReconcilePending(true);
    try {
      const path = `/clusters/${encodeURIComponent(cluster)}/controllers/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/reconcile`;
      await bffFetch(path, { method: "POST" });
      toast("Reconcile triggered");
      invalidateAll();
    } catch (e) {
      toast("Reconcile failed: " + (e instanceof Error ? e.message : "unknown"));
    }
    setReconcilePending(false);
  }, [cluster, namespace, name, toast, invalidateAll]);

  const stages = ctrl ? buildStages(ctrl) : [];

  /* ── Confirm state (5.3) ── */

  const [confirm, setConfirm] = useState<"restart" | "power-off" | "hibernate" | "reprovision" | null>(null);

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loadingBanner}>Loading controller...</div>
      </div>
    );
  }

  if (error instanceof ApiError && error.status === 403) return <ForbiddenPage />;
  if (error instanceof ApiError && error.status === 404) return <NotFoundPage />;
  if (error) return <GenericErrorPage />;
  if (!ctrl) return <NotFoundPage />;

  const runningBuilds = ctrl.observability?.summary?.runningBuilds ?? 0;

  /* ── Verdict derivation (5.2) ── */

  type Tone = "ok" | "warn" | "bad";
  let verdict: { tone: Tone; head: string; sub?: string; icon: string } | null = null;

  if (!ctrl.miteConnected) {
    const lastSeen = ctrl.lastSeen;
    const dur = lastSeen ? Math.floor((Date.now() - new Date(lastSeen).getTime()) / 60000) : null;
    const staleText = dur != null ? `${dur} minute${dur === 1 ? "" : "s"}` : "stale";
    verdict = {
      tone: "bad",
      head: `Mite disconnected — everything below is ${staleText}${dur != null ? " stale" : ""}`,
      sub: dur != null ? `Last heartbeat: ${new Date(lastSeen!).toLocaleString()}` : "No heartbeat received yet.",
      icon: "⏻",
    };
  } else if (ctrl.lastApplyResult?.succeeded === false) {
    const failedSections = ctrl.lastApplyResult.sections?.filter((s) => !s.ok) || [];
    const failedNames = failedSections.map((s) => s.name).join(", ") || "unknown";
    const plural = failedSections.length > 1;
    verdict = {
      tone: "bad",
      head: `Last apply failed — the ${failedNames} section${plural ? "s were" : " was"} rejected`,
      sub: `Jenkins is running the previous configuration.${runningBuilds > 0 ? ` ${runningBuilds} running build${runningBuilds !== 1 ? "s" : ""} will be terminated.` : ""}`,
      icon: "✗",
    };
  } else if (attachedBundle?.status?.phase === "Invalid") {
    verdict = {
      tone: "warn",
      head: "Bundle is invalid — configuration is frozen at the last good apply",
      sub: "",
      icon: "⚠",
    };
  } else if (ctrl.liveDrift?.detected) {
    verdict = {
      tone: "warn",
      head: "Configuration drift detected — live Jenkins state differs from the last apply",
      sub: "Reconcile to converge, or view the diff to see what changed.",
      icon: "⚠",
    };
  } else {
    verdict = {
      tone: "ok",
      head: "Healthy — desired state converged",
      sub: "",
      icon: "✓",
    };
  }

  const confirmBody: Record<string, string> = {
    restart: "Rolls the Jenkins pod. The controller is unreachable for roughly 40 seconds.",
    "power-off": "Stops the controller. Jenkins stops serving until powered back on.",
    hibernate: "Scales the controller to zero. It wakes on the next inbound request or manual wake.",
    reprovision: "Destroys and rebuilds the StatefulSet from spec. Job history on the PVC is retained; anything in the container filesystem is lost.",
  };

  const runningBuildsLine = runningBuilds === 1
    ? "1 running build will be terminated."
    : runningBuilds > 1
    ? `${runningBuilds} running builds will be terminated.`
    : "No running builds will be interrupted.";

  const confirmPending =
    confirm === "restart" ? restart.isPending
    : confirm === "power-off" || confirm === "hibernate" ? power.isPending
    : confirm === "reprovision" ? reprovision.isPending
    : false;

  const confirmProgressLabel: Record<string, string> = {
    restart: "Restarting…",
    "power-off": "Powering off…",
    hibernate: "Hibernating…",
    reprovision: "Reprovisioning…",
  };

  return (
    <div className={styles.page}>
      <Link to="/controllers" className={styles.back}>
        ← Back to controllers
      </Link>

      <ControllerHeader
        ctrl={ctrl}
        cluster={cluster}
        namespace={namespace}
        name={name}
        isCore={isCore}
        canManage={canManage}
        reloadConfig={reloadConfig}
        reconcile={reconcile}
        reconcilePending={reconcilePending}
        setConfirm={setConfirm}
        power={power}
      />

      <VersionRollBanner versionStatus={ctrl.versionStatus} />
      <ReconcileBlockedBanner reconcileBlocked={ctrl.reconcileBlocked} />
      <PluginConflictBanner pluginConflict={ctrl.pluginConflict} />
      {ctrl.pendingRestart && (
        <PendingRestartBanner
          pendingRestart={ctrl.pendingRestart}
          cluster={cluster}
          namespace={namespace}
          name={name}
          kind="drift"
        />
      )}
      {ctrl.pendingItemDeletions && ctrl.pendingItemDeletions.length > 0 && (
        <PendingDeletionBanner
          pendingDeletions={ctrl.pendingItemDeletions}
          cluster={cluster}
          namespace={namespace}
          name={name}
        />
      )}

      {verdict && (
        <VerdictStrip
          verdict={verdict}
          ctrl={ctrl}
          cluster={cluster}
          namespace={namespace}
          name={name}
          canUpdate={canUpdate}
          attachedBundle={attachedBundle}
          detachMutation={detachMutation}
          reloadConfig={reloadConfig}
          reconcile={reconcile}
          reconcilePending={reconcilePending}
          setActiveTab={setActiveTab}
        />
      )}

      {confirm && (
        <div className={styles.headerConfirm} role="alertdialog" aria-label={`Confirm ${confirm}`}>
          <span>{confirmBody[confirm]}</span>
          <span className={styles.muted}>{runningBuildsLine}</span>
          <div className={styles.confirmActions}>
            <Button size="sm" variant="primary" disabled={confirmPending} onClick={() => {
              const onSettled = () => setConfirm(null);
              if (confirm === "restart") restart.mutate(undefined, { onSettled });
              else if (confirm === "power-off") power.mutate("Stopped", { onSettled });
              else if (confirm === "hibernate") power.mutate("Hibernated", { onSettled });
              else if (confirm === "reprovision") reprovision.mutate(undefined, { onSettled });
            }}>
              {confirmPending ? confirmProgressLabel[confirm] : `Yes, ${confirm === "power-off" ? "power off" : confirm}`}
            </Button>
            <Button size="sm" variant="ghost" disabled={confirmPending} onClick={() => setConfirm(null)}>Cancel</Button>
          </div>
        </div>
      )}

      <Tabs tabs={TABS} activeTab={activeTab} onSelect={setActiveTab} />

      {activeTab === "overview" && <OverviewTab ctrl={ctrl} stages={stages} runningBuilds={runningBuilds} canUpdate={canUpdate} />}
      {activeTab === "configuration" && (
        <ConfigurationTab
          ctrl={ctrl}
          cluster={cluster}
          namespace={namespace}
          name={name}
          stages={stages}
          reloadConfig={reloadConfig}
          setConfirm={setConfirm}
          canUpdate={canUpdate}
        />
      )}
      {activeTab === "observability" && <ObservabilityTab ctrl={ctrl} />}
      {activeTab === "plugins" && <PluginsTab ctrl={ctrl} />}
      {activeTab === "diagnostics" && <DiagnosticsTab ctrl={ctrl} cluster={cluster} namespace={namespace} name={name} isCore={isCore} />}
    </div>
  );
}

function ControllerHeader({
  ctrl, cluster, namespace, name, isCore,
  canManage, reloadConfig, reconcile, reconcilePending, setConfirm, power,
}: {
  ctrl: ControllerData;
  cluster: string;
  namespace: string;
  name: string;
  isCore: boolean;
  canManage: boolean;
  reloadConfig: { mutate: () => void; isPending: boolean };
  reconcile: () => Promise<void>;
  reconcilePending: boolean;
  setConfirm: (c: "restart" | "power-off" | "hibernate" | "reprovision" | null) => void;
  power: { mutate: (state: "Running" | "Stopped" | "Hibernated") => void; isPending: boolean };
}) {
  const stopped = ctrl.powerState === "Stopped" || ctrl.phase === "Stopped";
  const hibernated = ctrl.powerState === "Hibernated" || ctrl.phase === "Hibernated";
  const [overflowOpen, setOverflowOpen] = useState(false);
  const overflowRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const [focusIndex, setFocusIndex] = useState(0);

  const lifecycleItems: { label: string; sub?: string; action: () => void }[] = [];
  const disruptiveItems: { label: string; sub?: string; action: () => void }[] = [];

  if (canManage) {
    lifecycleItems.push({ label: "Reload configuration", sub: "no restart", action: () => { reloadConfig.mutate(); setOverflowOpen(false); } });
    if (!stopped && !hibernated) {
      lifecycleItems.push({ label: "Hibernate", sub: "reversible", action: () => { setConfirm("hibernate"); setOverflowOpen(false); } });
    }
    disruptiveItems.push({ label: "Restart", sub: "rolls pod", action: () => { setConfirm("restart"); setOverflowOpen(false); } });
    if (!stopped && !hibernated) {
      disruptiveItems.push({ label: "Power off", sub: "stops serving", action: () => { setConfirm("power-off"); setOverflowOpen(false); } });
    }
    disruptiveItems.push({ label: "Reprovision", sub: "rebuilds", action: () => { setConfirm("reprovision"); setOverflowOpen(false); } });
  }

  useEffect(() => {
    if (!overflowOpen) {
      triggerRef.current?.focus();
      setFocusIndex(0);
    } else {
      const first = overflowRef.current?.querySelector('[role="menuitem"]') as HTMLElement | null;
      first?.focus();
    }
  }, [overflowOpen]);

  useEffect(() => {
    if (!overflowOpen) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") { setOverflowOpen(false); return; }
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const items = overflowRef.current?.querySelectorAll('[role="menuitem"]');
        if (!items || items.length === 0) return;
        const dir = e.key === "ArrowDown" ? 1 : -1;
        const next = (focusIndex + dir + items.length) % items.length;
        setFocusIndex(next);
        (items[next] as HTMLElement).focus();
      }
    };
    const handleClickOutside = (e: MouseEvent) => {
      if (overflowRef.current && !overflowRef.current.contains(e.target as Node) &&
          triggerRef.current && !triggerRef.current.contains(e.target as Node)) {
        setOverflowOpen(false);
      }
    };
    document.addEventListener("keydown", handleKeyDown);
    document.addEventListener("mousedown", handleClickOutside);
    return () => { document.removeEventListener("keydown", handleKeyDown); document.removeEventListener("mousedown", handleClickOutside); };
  }, [overflowOpen, focusIndex]);

  const showWakePower = stopped || hibernated;

  return <header className={styles.detailHero}>
    <div className={styles.ctrlIc}>⬡</div>
    <div className={styles.heroMeta}>
      <h1>{ctrl.name} <StatusPill phase={ctrl.phase} /></h1>
      <div className={styles.sub}>
        <span>cluster <b className={styles.mono}>{cluster}</b></span>
        <span>namespace <b className={styles.mono}>{namespace}</b></span>
        <span><Pulse active={ctrl.miteConnected} size={8} /> mite <b>{ctrl.miteConnected ? "connected" : "offline"}</b></span>
        <span>Jenkins <b>{ctrl.jenkinsHealth || "unknown"}</b></span>
        {ctrl.miteImageStatus && (
          <span>mite image <b>{ctrl.miteImageStatus.stale === undefined ? "unknown" : ctrl.miteImageStatus.stale ? "stale" : "current"}</b></span>
        )}
        <span>version <b className={styles.mono}>{ctrl.jenkinsVersion || ctrl.version || "—"}</b></span>
      </div>
    </div>
    <div className={styles.heroActions}>
      {showWakePower && (
        <Button size="sm" variant="primary" onClick={() => power.mutate("Running")}>
          {hibernated ? "Wake" : "Power On"}
        </Button>
      )}
      {ctrl.endpoint ? (
        <a href={ctrl.endpoint} target="_blank" rel="noreferrer">
          <Button variant="primary" size="sm">Open Jenkins</Button>
        </a>
      ) : (
        <Button size="sm" disabled>Not ready</Button>
      )}
      {isCore && ctrl.routingMode === "path" && (
        <Link to={`${controllerRoute(cluster, namespace, name)}/jenkins`}>
          <Button variant="ghost" size="sm">View embedded</Button>
        </Link>
      )}
      <ReconcileButton onReconcile={reconcile} pending={reconcilePending} />
      {canManage && (
        <div className={styles.overflowRelative}>
          <Button
            ref={triggerRef}
            variant="ghost"
            size="sm"
            aria-label="More actions"
            aria-haspopup="menu"
            aria-expanded={overflowOpen}
            onClick={() => setOverflowOpen((o) => !o)}
          >
            ⋯
          </Button>
          {overflowOpen && (
            <div className={styles.overflowPop} ref={overflowRef} role="menu">
              <div className={styles.overflowGroupLabel}>Lifecycle</div>
              {lifecycleItems.map((item) => (
                <button
                  key={item.label}
                  role="menuitem"
                  className={styles.menuItemBtn}
                  onClick={item.action}
                >
                  <div className={styles.menuItemLabel}>{item.label}</div>
                  {item.sub && <div className={styles.menuItemSub}>{item.sub}</div>}
                </button>
              ))}
              <div className={styles.overflowGroupLabel}>Disruptive</div>
              {disruptiveItems.map((item) => (
                <button
                  key={item.label}
                  role="menuitem"
                  className={styles.menuItemBtn}
                  onClick={item.action}
                >
                  <div className={styles.menuItemLabel}>{item.label}</div>
                  {item.sub && <div className={styles.menuItemSub}>{item.sub}</div>}
                </button>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  </header>;
}

function OverviewTab({
  ctrl,
  stages,
  runningBuilds,
  canUpdate,
}: {
  ctrl: NonNullable<ReturnType<typeof useController>["data"]>;
  stages: ReturnType<typeof buildStages>;
  runningBuilds: number;
  canUpdate: boolean;
}) {
  const drift = versionsDiffer(ctrl.jenkinsVersion, ctrl.version);
  const miteConnected = ctrl.miteConnected;
  const jobs = ctrl.observability?.summary?.totalJobs ?? "—";
  const rbs = ctrl.observability?.summary?.runningBuilds ?? 0;
  return (
    <div>
      <div className={styles.tiles}>
        <div className={styles.tile}>
          <div className={styles.tileK}>Jenkins version</div>
          <div className={styles.tileV}>{ctrl.jenkinsVersion || "—"}</div>
          <div className={styles.tileSub}>{drift ? <span className={styles.driftText}>desired {ctrl.version}</span> : "matches desired"}</div>
        </div>
        <div className={styles.tile}>
          <div className={styles.tileK}>Jobs</div>
          <div className={styles.tileV}>{jobs}</div>
          <div className={styles.tileSub}>{miteConnected ? "reported by mite" : "unknown — mite offline"}</div>
        </div>
        <div className={styles.tile}>
          <div className={styles.tileK}>Running builds</div>
          <div className={styles.tileV}>{rbs > 0 ? `${rbs} in flight` : rbs}</div>
          <div className={styles.tileSub}>{miteConnected ? (rbs > 0 ? "a restart would terminate these" : "idle") : "unknown — mite offline"}</div>
        </div>
        <div className={styles.tile}>
          <div className={styles.tileK}>Last reconciled</div>
          <div className={styles.tileV}>{ctrl.lastReconciledAt ? <span title={new Date(ctrl.lastReconciledAt).toLocaleString()}>{age(ctrl.lastReconciledAt)}</span> : "—"}</div>
          <div className={styles.tileSub}>every {ctrl.reconciliationPolicy?.interval || "30s"} · {ctrl.reconciliationPolicy?.mode || "automatic"}</div>
        </div>
      </div>
      <ConfigPipeline
        stages={stages}
        runningBuilds={runningBuilds}
        lastApplySections={ctrl.lastApplyResult?.sections}
      />
      <div className={styles.twoCol}>
        <div className={styles.col}>
          <Card title="Spec">
            <KVGrid
              items={[
                { key: "Endpoint", value: ctrl.endpoint || "—" },
                { key: "Namespace", value: ctrl.namespace },
                { key: "Reconciliation", value: `${ctrl.reconciliationPolicy?.mode || "automatic"} · every ${ctrl.reconciliationPolicy?.interval || "30s"}` },
              ]}
            />
          </Card>
        </div>
        <div className={styles.col}>
          <BundleCard ctrl={ctrl} canUpdate={canUpdate} />
        </div>
      </div>
    </div>
  );
}

// ---- Bundle attach / change / detach card ----

function BundleCard({ ctrl, canUpdate }: { ctrl: NonNullable<ReturnType<typeof useController>["data"]>; canUpdate: boolean }) {
  const { toast } = useToast();
  const qc = useQueryClient();
  const cluster = ctrl.cluster;
  const ns = ctrl.namespace;
  const name = ctrl.name;

  const [showSelector, setShowSelector] = useState(false);
  const [selectedBundle, setSelectedBundle] = useState<string | null>(null);

  // Display identity comes from effectiveBundle, which is already namespace-
  // qualified. Using the controller's own namespace treated an explicit
  // cross-namespace composedBundleRef as local — health badge and link both
  // pointed at the wrong place — and a zero-config controller, whose starter
  // lives in the operator namespace, resolved to nothing at all.
  //
  // The raw composedBundleRef is still what the attach/detach PATCH writes; only
  // display and links read this.
  const bundleNS =
    ctrl.effectiveBundle?.namespace ?? ctrl.composedBundleRef?.namespace ?? ns;
  const bundleDisplayName =
    ctrl.effectiveBundle?.name ?? ctrl.composedBundleRef?.name;

  // Two queries, deliberately. The attached bundle can live in another namespace
  // (an explicit cross-namespace ref, or the operator-namespace starter), while
  // BundleSelector below only offers bundles from THIS controller's namespace.
  // Serving both from one query made the selector's validity check look up the
  // picked name in the wrong namespace and report every valid choice invalid.
  const { data: attachedBundlesData } = useComposedBundles(cluster, bundleNS);
  const { data: selectableBundlesData } = useComposedBundles(cluster, ns);

  const attachedBundle = attachedBundlesData?.items?.find(
    (b) => b.metadata.name === bundleDisplayName
  ) ?? null;

  const attachedPhase = attachedBundle?.status?.phase;
  const attachedErrors = attachedBundle?.status?.errors ?? [];
  const attachedWarnings = attachedBundle?.status?.warnings ?? [];
  const isAttachedReady = !attachedPhase || attachedPhase === "Ready";

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["controllers"] });
    qc.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
    qc.invalidateQueries({ queryKey: ["composed-bundles", cluster, ns] });
  };

  const feedback = (ok: string) => ({
    onSuccess: () => { invalidate(); toast(ok); setShowSelector(false); setSelectedBundle(null); },
    onError: (e: Error) => toast(`Failed: ${e.message}`),
  });

  const attachMutation = useMutation({
    mutationFn: (bundleName: string) =>
      updateController(cluster, name, ns, { spec: { composedBundleRef: { name: bundleName } } }),
    ...feedback("Bundle attached"),
  });

  const detachMutation = useMutation({
    mutationFn: () =>
      updateController(cluster, name, ns, { spec: { composedBundleRef: null } }),
    ...feedback("Bundle detached"),
  });

  // Attach and Change are the same PATCH (set composedBundleRef to the picked
  // bundle); the surrounding UI differs but the mutation is identical.
  const handleSetBundle = () => {
    if (!selectedBundle) return;
    attachMutation.mutate(selectedBundle);
  };

  const handleDetach = () => detachMutation.mutate();

  const hasBundle = !!ctrl.composedBundleRef?.name;
  // A controller that names no bundle is not unconfigured: it runs the operator's
  // built-in starter. effectiveBundle is read-only and reports which one, so the
  // card can say so instead of "No bundle attached".
  const starterBundle = ctrl.effectiveBundle?.builtIn ? ctrl.effectiveBundle : null;

  return (
    <Card title="Bundle">
      {hasBundle ? (
        // ---- Bundle attached ----
        <div className={styles.bundleSection}>
          <div className={styles.bundleHeader}>
            <Link
              to={`/catalog/bundles/${bundleNS}/${bundleDisplayName}${clusterQuery(cluster)}`}
              className={styles.refLink}
            >
              <span className={styles.bundleName}>{bundleDisplayName}</span>
            </Link>
            <BundleHealthBadge phase={attachedPhase} />
          </div>

          {/* Errors / warnings for non-Ready bundle */}
          {!isAttachedReady && (
            <div className={styles.bundleIssues}>
              {attachedErrors.map((err, i) => (
                <div key={`err-${i}`} className={styles.bundleError}>⛔ {err}</div>
              ))}
              {attachedWarnings.map((w, i) => (
                <div key={`warn-${i}`} className={styles.bundleWarning}>⚠ {w}</div>
              ))}
            </div>
          )}

          {showSelector ? (
            <div className={styles.bundleSelectArea}>
              <BundleSelector
                cluster={cluster}
                namespace={ns}
                value={selectedBundle}
                onChange={setSelectedBundle}
              />
              <div className={styles.bundleSelectActions}>
                <Button
                  size="sm"
                  variant="primary"
                  disabled={!selectedBundle || attachMutation.isPending}
                  onClick={handleSetBundle}
                >
                  {attachMutation.isPending ? "Changing..." : "Confirm Change"}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => { setShowSelector(false); setSelectedBundle(null); }}
                >
                  Cancel
                </Button>
              </div>
            </div>
          ) : canUpdate ? (
            <div className={styles.bundleActions}>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => { setShowSelector(true); setSelectedBundle(null); }}
              >
                Change
              </Button>
              <Button
                size="sm"
                variant="ghost"
                className={styles.dangerLink}
                disabled={detachMutation.isPending}
                onClick={handleDetach}
              >
                {detachMutation.isPending ? "Detaching..." : "Detach"}
              </Button>
            </div>
          ) : null}
        </div>
      ) : canUpdate ? (
        // ---- No bundle attached ----
        <div className={styles.bundleSection}>
          <div className={styles.bundleEmptyText}>
            {starterBundle ? (
              <>
                Using the built-in{" "}
                <Link
                  to={`/catalog/bundles/${bundleNS}/${bundleDisplayName}${clusterQuery(cluster)}`}
                  className={styles.refLink}
                >
                  starter bundle
                </Link>
                . Select one below to replace it with your own.
              </>
            ) : (
              "No bundle attached. Select one below to attach."
            )}
          </div>

          <BundleSelector
            cluster={cluster}
            namespace={ns}
            value={selectedBundle}
            onChange={setSelectedBundle}
          />

          {/* Show warning if attaching an Invalid bundle */}
          {selectedBundle && (() => {
            const sel = selectableBundlesData?.items?.find((b) => b.metadata.name === selectedBundle);
            if (sel?.status?.phase === "Invalid") {
              return (
                <div className={styles.bundleIssues}>
                  <div className={styles.bundleWarning}>
                    ⚠ This bundle is Invalid — attaching it is allowed but the controller will not reconcile until the bundle is fixed.
                  </div>
                  {sel.status.errors?.map((e, i) => (
                    <div key={`err-${i}`} className={styles.bundleError}>⛔ {e}</div>
                  ))}
                </div>
              );
            }
            return null;
          })()}

          <Button
            size="sm"
            variant="primary"
            disabled={!selectedBundle || attachMutation.isPending}
            onClick={handleSetBundle}
          >
            {attachMutation.isPending ? "Attaching..." : "Attach Bundle"}
          </Button>
        </div>
      ) : (
        <div className={styles.bundleSection}>
          <div className={styles.bundleEmptyText}>
            {starterBundle
              ? `Using the built-in starter bundle (${starterBundle.namespace}/${starterBundle.name}).`
              : "No bundle attached."}
          </div>
        </div>
      )}
    </Card>
  );
}

// ---- Resource override / overlay editor (admin-only) ----
function ObservabilityTab({ ctrl }: { ctrl: ControllerData }) {
  return <div data-testid="observability-tab"><ObservabilityPanel observability={ctrl.observability} /></div>;
}

function DiagnosticsTab({ ctrl, cluster, namespace, name, isCore }: {
  ctrl: ControllerData; cluster: string; namespace: string; name: string; isCore: boolean;
}) {
  return (
    <div className={styles.diagnosticTabs} data-testid="diagnostics-tab">
      <div className={styles.twoCol}>
        <Card title="Conditions">{ctrl.conditions && ctrl.conditions.length > 0 ? <div className={styles.conditions}>{ctrl.conditions.map((condition) => <div className={styles.condItem} key={condition.type}><div className={`${styles.condDot} ${condition.status === "True" ? styles.softOk : styles.softBad}`} /><div><div className={styles.ft}><b>{condition.type}</b> · {condition.status}</div><div className={styles.fm}>{condition.reason || condition.message || "—"}</div></div></div>)}</div> : <p className={styles.muted}>No conditions reported</p>}</Card>
        <Card title="Connection details"><KVGrid items={[{ key: "Last heartbeat", value: ctrl.lastSeen || "—" }, { key: "Certificate expiry", value: ctrl.certExpiry || "—" }]} /></Card>
      </div>
      <LogsCard cluster={cluster} namespace={namespace} name={name} isCore={isCore} />
      <ActivityTab cluster={cluster} namespace={namespace} name={name} />
    </div>
  );
}

// --- Logs card (always mounted under Diagnostics) ---
const LOG_LEVELS = ["All", "Operator", "Mite", "Jenkins"] as const;

interface LogEntry {
  timestamp?: string;
  level?: string;
  source?: string;
  message: string;
}

function LogsCard({ cluster, namespace, name, isCore }: { cluster: string; namespace: string; name: string; isCore: boolean }) {
  const [logLevel, setLogLevel] = useState<string>("All");
  const [lines, setLines] = useState<LogEntry[]>([]);
  const [paused, setPaused] = useState(false);
  const [height, setHeight] = useState(() => {
    try {
      const v = parseInt(localStorage.getItem("varroa-controller-log-height") || "");
      return isNaN(v) ? 380 : Math.max(160, Math.min(900, v));
    } catch { return 380; }
  });
  const paneRef = useRef<HTMLDivElement>(null);

  const { lastEvent, readyState, error: streamError } = useEventStream<LogEntry>(
    isCore && !paused
      ? `${BFF_BASE}/clusters/${encodeURIComponent(cluster)}/controllers/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/logs?follow=true`
      : null,
    isCore && !paused ? `controller:${cluster}/${namespace}/${name}` : null,
  );

  useEffect(() => {
    if (lastEvent) {
      const entry = lastEvent as unknown as LogEntry;
      if (entry && typeof entry.message === "string") {
        setLines((prev) => {
          const next = [...prev, entry];
          return next.length > 500 ? next.slice(-500) : next;
        });
      }
    }
  }, [lastEvent]);

  const filtered = lines
    .filter((l) => logLevel === "All" || l.source?.toLowerCase() === logLevel.toLowerCase())
    .map((l) => ({
      timestamp: l.timestamp || new Date().toISOString(),
      level: (l.level || "INFO") as "INFO" | "WARN" | "ERROR" | "DEBUG" | "OK",
      source: l.source || "operator",
      message: l.message,
    }));

  if (!isCore) {
    return (
      <Card title="Logs">
        <div className={styles.logsEmpty}>
          Logs are served only for controllers on the core cluster
        </div>
      </Card>
    );
  }

  const handlePointerDown = (e: React.PointerEvent) => {
    const el = paneRef.current;
    if (!el) return;
    el.setPointerCapture(e.pointerId);
    const startY = e.clientY;
    const startH = el.getBoundingClientRect().height;
    const onMove = (ev: PointerEvent) => {
      const newH = Math.max(160, Math.min(900, startH + (ev.clientY - startY)));
      el.style.height = `${newH}px`;
    };
    const onUp = (ev: PointerEvent) => {
      el.releasePointerCapture(ev.pointerId);
      const finalH = Math.max(160, Math.min(900, startH + (ev.clientY - startY)));
      setHeight(finalH);
      localStorage.setItem("varroa-controller-log-height", String(finalH));
      document.removeEventListener("pointermove", onMove);
      document.removeEventListener("pointerup", onUp);
    };
    document.addEventListener("pointermove", onMove);
    document.addEventListener("pointerup", onUp);
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    let newH = height;
    if (e.key === "ArrowUp") { newH = Math.min(900, height + 21); e.preventDefault(); }
    else if (e.key === "ArrowDown") { newH = Math.max(160, height - 21); e.preventDefault(); }
    else if (e.key === "Home") { newH = 160; e.preventDefault(); }
    else if (e.key === "End") { newH = 900; e.preventDefault(); }
    if (newH !== height) {
      setHeight(newH);
      localStorage.setItem("varroa-controller-log-height", String(newH));
    }
  };

  return (
    <Card
      title="Logs"
      headerRight={
        <div className={styles.logsHeaderRight}>
          {LOG_LEVELS.map((lv) => (
            <button
              key={lv}
              className={`${styles.lvl} ${logLevel === lv ? styles.lvlOn : ""}`}
              onClick={() => setLogLevel(lv)}
            >
              {lv}
            </button>
          ))}
          <button
            className={styles.toggleBtn}
            aria-pressed={paused}
            onClick={() => { setPaused(!paused); if (!paused) setLines([]); }}
          >
            {paused ? "▶ Resume" : "⏸ Pause"}
          </button>
          {(() => {
            const streamState = paused
              ? "paused"
              : readyState === "open"
              ? "live"
              : streamError
              ? "error"
              : readyState === "connecting"
              ? "reconnecting"
              : "closed";
            return (
              <>
                <span className={styles.streamState} data-state={streamState}>{streamState}</span>
                {streamState === "error" && (
                  <span className={styles.streamErrorReason} title={streamError!.message}>
                    {streamError!.message}
                  </span>
                )}
              </>
            );
          })()}
        </div>
      }
    >
      <div ref={paneRef} data-testid="log-pane" style={{ height, overflow: "auto", position: "relative" }}>
        <Console lines={filtered} />
      </div>
      <div
        role="separator"
        aria-orientation="horizontal"
        aria-valuemin={160}
        aria-valuemax={900}
        aria-valuenow={height}
        tabIndex={0}
        aria-label="Resize log pane"
        onPointerDown={handlePointerDown}
        onKeyDown={handleKeyDown}
        className={styles.grip}
      />
    </Card>
  );
}

// --- Activity Tab (scoped timeline) ---

function ActivityTab({ cluster, namespace, name }: { cluster: string; namespace: string; name: string }) {
  return (
    <ActivityTimeline scope={{ cluster, namespace, name }} />
  );
}

// ---- Hash Disclosure ----

function HashDisclosure({ ctrl }: { ctrl: ControllerData }) {
  const { toast } = useToast();
  const inSync = ctrl.lastApplyResult?.hash === ctrl.desiredStateHash && ctrl.lastApplyResult?.succeeded === true;
  const hashes: { label: string; hash: string }[] = [];
  if (ctrl.configHash) hashes.push({ label: "configHash", hash: ctrl.configHash });
  if (ctrl.rbacHash) hashes.push({ label: "rbacHash", hash: ctrl.rbacHash });
  if (ctrl.desiredStateHash) hashes.push({ label: "desiredStateHash", hash: ctrl.desiredStateHash });
  if (ctrl.appliedBundleHash) hashes.push({ label: "appliedBundleHash", hash: ctrl.appliedBundleHash });

  return (
    <details className={styles.hashDisclosure}>
      <summary className={styles.hashSummary}>
        <span className={styles.syncChip} data-sync={inSync}>
          {inSync ? "✓ In sync" : "⟳ Out of sync"}
        </span>
        <span className={styles.hashCount}>{hashes.length} hashes</span>
      </summary>
      <div className={styles.hashList}>
        {hashes.map((h) => (
          <div key={h.label} className={styles.statLine}>
            <span className={styles.sk}>{h.label}</span>
            <span className={styles.hash}>{h.hash.slice(0, 12)}…</span>
            <button
              aria-label={`Copy ${h.label}`}
              className={styles.copyBtn}
              onClick={async () => { try { await navigator.clipboard.writeText(h.hash); toast("Copied"); } catch {} }}
            >
              Copy
            </button>
          </div>
        ))}
      </div>
    </details>
  );
}

// --- Mite tab (live SSE data) ---
// --- Configuration Tab ---

function ConfigurationTab({
  ctrl, cluster, namespace, name,
  stages, reloadConfig, setConfirm, canUpdate,
}: {
  ctrl: NonNullable<ReturnType<typeof useController>["data"]>;
  cluster: string;
  namespace: string;
  name: string;
  stages: ReturnType<typeof buildStages>;
  reloadConfig: { mutate: () => void; isPending: boolean };
  setConfirm: (c: "restart" | "power-off" | "hibernate" | "reprovision" | null) => void;
  canUpdate: boolean;
}) {
  const { diff, loading: diffLoading, error: diffError, fetchDiff } = useControllerDiff(cluster, namespace, name);
  const { permissions } = useAuth();
  const queryClient = useQueryClient();
  const [editingPolicy, setEditingPolicy] = useState(false);

  const runningBuilds = ctrl.observability?.summary?.runningBuilds;

  return (
    <div className={styles.configuration} data-testid="configuration-tab">
      <ConfigPipeline
      stages={stages}
      applyHistory={ctrl.applyHistory}
      diff={diff}
      diffLoading={diffLoading}
      diffError={diffError}
      onFetchDiff={fetchDiff}
      runningBuilds={runningBuilds}
      lastApplySections={ctrl.lastApplyResult?.sections}
      onReload={canDoInNamespace(permissions, ctrl.namespace, "controllers", "approve-restart") ? () => reloadConfig.mutate() : undefined}
      onRestart={canDoInNamespace(permissions, ctrl.namespace, "controllers", "approve-restart") ? () => setConfirm("restart") : undefined}
      onReprovision={canDoInNamespace(permissions, ctrl.namespace, "controllers", "manage") ? () => setConfirm("reprovision") : undefined}
      />
      <Card title="Reconciliation state">
        <KVGrid items={[{ key: "Mode", value: ctrl.reconciliationPolicy?.mode || "automatic" }, { key: "Interval", value: ctrl.reconciliationPolicy?.interval || "30s (default)" }]} />
        <HashDisclosure ctrl={ctrl} />
        {canUpdate && <Button size="sm" variant="ghost" onClick={() => setEditingPolicy((value) => !value)}>{editingPolicy ? "Cancel" : "Edit policy"}</Button>}
        {editingPolicy && <PolicyEditForm policy={ctrl.reconciliationPolicy} cluster={cluster} namespace={namespace} name={name} onDone={() => { setEditingPolicy(false); queryClient.invalidateQueries({ queryKey: ["controller", cluster, namespace, name] }); }} />}
      </Card>
      <div className={styles.configurationEditors}>
        <VersionCard ctrl={ctrl} canUpdate={canUpdate} />
        <HibernationCard ctrl={ctrl} cluster={cluster} ns={namespace} name={name} canUpdate={canUpdate} />
        <ProbesCard ctrl={ctrl} cluster={cluster} ns={namespace} name={name} canUpdate={canUpdate} />
        {/*
          initialIngressSpec/initialMiteSpec are intentionally omitted: the
          BFF's ControllerDetail projection doesn't return spec.ingressSpec or
          spec.miteSpec today (see hooks/useControllers.ts), so there's
          nothing to pre-populate the YAML tiers with yet. The tiers still
          work for setting new values; pre-population is a follow-up once the
          BFF projects those fields (issue #429 follow-up).
        */}
        <SpecEditorCard cluster={cluster} ns={namespace} name={name} initialOverlay={ctrl.resourceOverlay} initialPodOverrides={ctrl.podOverrides} canUpdate={canUpdate} />
      </div>
    </div>
  );
}

// --- Pending Restart Banner ---

// VersionCard is the post-create Jenkins version editor. It reuses the shared
// VersionPicker + catalog badges, surfaces the upgrade/EOL indicators and a
// plugin-set change preview, and applies the change through the existing PATCH
// endpoint — surfacing B's preflight failures inline (no client-side gate).
export function VersionCard({ ctrl, canUpdate }: { ctrl: NonNullable<ReturnType<typeof useController>["data"]>; canUpdate?: boolean }) {
  const upd = canUpdate !== false; // default to true when not specified
  const { toast } = useToast();
  const qc = useQueryClient();
  const cluster = ctrl.cluster;
  const ns = ctrl.namespace;
  const name = ctrl.name;

  const { data: cfg } = useQuery({
    queryKey: ["provisioning-config", cluster],
    queryFn: () => getProvisioningConfig(cluster),
  });
  const versions = cfg?.versions ?? [];
  const { data: profiles } = useQuery({
    queryKey: ["version-profiles", cluster],
    queryFn: () => getVersionProfiles(cluster),
  });

  const [target, setTarget] = useState(ctrl.version || "");
  const [failedChecks, setFailedChecks] = useState<PreflightCheck[]>([]);
  const [conflicts, setConflicts] = useState<import("../api/client").ConflictInfo[]>([]);
  const [showConflict, setShowConflict] = useState(false);

  const inertImg = overlayJenkinsImage(ctrl.resourceOverlay?.statefulSet);
  const inert = !!inertImg;

  const apply = useMutation({
    mutationFn: (force: boolean) => updateController(cluster, name, ns, { spec: { version: target } }, { force }),
    onSuccess: () => {
      setFailedChecks([]);
      setConflicts([]);
      setShowConflict(false);
      qc.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      qc.invalidateQueries({ queryKey: ["controllers"] });
      toast(`Version change to ${target} applied`);
    },
    onError: (e: Error) => {
      if (e instanceof ControllerConflictError) {
        setConflicts(e.conflicts);
        setShowConflict(true);
        return;
      }
      const checks = parsePreflightChecks(e);
      if (checks) {
        setFailedChecks(checks.filter((c) => c.status === "fail"));
      } else {
        toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
      }
    },
  });

  const info = upgradeInfo(ctrl.version || "", versions);
  const cur = versions.find((v) => v.version === ctrl.version);
  const tgt = versions.find((v) => v.version === target);
  const changed = target !== "" && target !== ctrl.version;

  // In-progress chip mirrors the async roll state (A's VersionRollStarted).
  const vs = ctrl.versionStatus;
  const rolling = vs?.rollPending === true && vs.rollReason === "VersionRollStarted";

  // Plugin-count preview (only when both catalog entries carry a count).
  let pluginNote: string | null = null;
  if (cur?.pluginCount !== undefined && tgt?.pluginCount !== undefined) {
    const delta = tgt.pluginCount - cur.pluginCount;
    const sign = delta >= 0 ? "+" : "";
    pluginNote = `Pinned plugins: ${cur.pluginCount} → ${tgt.pluginCount} (Δ${sign}${delta})`;
  } else if (tgt?.pluginCount !== undefined) {
    pluginNote = `Target pins ${tgt.pluginCount} plugins`;
  }
  // Readiness warning (metadata-only detected via pluginSetReady ABSENCE).
  let readinessWarn: string | null = null;
  if (tgt && tgt.pluginSetReady === undefined) {
    readinessWarn = "target has no pinned plugin set; the embedded baseline set will be used";
  } else if (tgt?.pluginSetReady === false) {
    readinessWarn = "target plugin set not yet materialized";
  }

  // Line-item plugin diff (coordinator-directed amendment): when D exposes a full
  // pinned plugin list (plugins[]) for BOTH the current-version-matching profile
  // and the target profile, diff them client-side. Profiles are matched to a
  // version with the same dotted-prefix rule as the drift indicator, so an LTS
  // line profile (2.555) matches a 3-segment patch (2.555.1).
  const matchProfile = (ver?: string) =>
    ver ? (profiles ?? []).find((p) => !versionsDiffer(p.version, ver)) : undefined;
  const curProfile = matchProfile(ctrl.version);
  const tgtProfile = matchProfile(target);
  const diff =
    changed && curProfile?.plugins && tgtProfile?.plugins
      ? pluginDiff(curProfile.plugins, tgtProfile.plugins)
      : null;
  const hasDiffContent =
    !!diff && (diff.added.length > 0 || diff.removed.length > 0 || diff.changed.length > 0);

  return (
    <>
    <Card title="Version" headerRight={<span className={styles.cardNote}>Saving rolls the pod</span>}>
      <div className={styles.versionCard}>
        {info.managed && (info.recommendedUpgrade || info.eol) && (
          <div className={styles.versionBadges}>
            {info.recommendedUpgrade && (
              <span className={styles.upgradeChip}>Upgrade available → {info.recommendedUpgrade}</span>
            )}
            {info.eol && (
              <span className={info.eolPassed ? styles.eolChipPast : styles.eolChip}>EOL {info.eol}</span>
            )}
          </div>
        )}
        {!info.managed && ctrl.version && (
          <div className={styles.versionMutedNote}>unmanaged version (no matching profile in the catalog)</div>
        )}
        {rolling && <div className={styles.inProgressChip}>⟳ Upgrade in progress…</div>}

        {inert ? (
          <div className={styles.versionMutedNote}>
            Version is pinned by a resourceOverlay statefulSet image override (
            <span className={styles.mono}>{inertImg}</span>). Edit the overlay to change the Jenkins image.
          </div>
        ) : upd ? (
          <VersionPicker versions={versions} value={target} onChange={setTarget} disabled={inert} />
        ) : (
          <div className={styles.versionMutedNote}>Version: {ctrl.version || "—"} (read-only)</div>
        )}

        {changed && !inert && (
          <>
            {pluginNote && <div className={styles.versionNote}>{pluginNote}</div>}
            {readinessWarn && <div className={styles.versionWarn}>⚠ {readinessWarn}</div>}
            {diff ? (
              hasDiffContent ? (
                <div className={styles.pluginDiff}>
                  {diff.added.map((id) => (
                    <div key={`a-${id}`} className={styles.pluginDiffAdd}>
                      + {id}
                    </div>
                  ))}
                  {diff.removed.map((id) => (
                    <div key={`r-${id}`} className={styles.pluginDiffRemove}>
                      − {id}
                    </div>
                  ))}
                  {diff.changed.map((c) => (
                    <div key={`c-${c.id}`} className={styles.pluginDiffChange}>
                      ~ {c.id}: {c.from} → {c.to}
                    </div>
                  ))}
                </div>
              ) : (
                <div className={styles.versionMutedNote}>No pinned-plugin changes.</div>
              )
            ) : (
              <div className={styles.versionMutedNote}>A per-plugin diff is not available.</div>
            )}
          </>
        )}

        {failedChecks.length > 0 && (
          <div className={styles.versionChecks}>
            {failedChecks.map((c, i) => (
              <div key={i} className={styles.versionCheckFail}>
                ✕ {c.message}
              </div>
            ))}
          </div>
        )}

        {upd && <div className={styles.versionActions}>
          <Button
            size="sm"
            variant="primary"
            disabled={!changed || inert || apply.isPending}
            onClick={() => apply.mutate(false)}
          >
            {apply.isPending ? "Applying…" : "Apply version"}
          </Button>
        </div>}
      </div>
    </Card>
    <ConflictDialog
      conflicts={conflicts}
      open={showConflict}
      onReload={() => {
        setShowConflict(false);
        setConflicts([]);
        qc.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      }}
      onOverride={() => apply.mutate(true)}
    />
    </>
  );
}

// ---- Health probes card ----

function ProbesCard({
  ctrl,
  cluster,
  ns,
  name,
  canUpdate,
}: {
  ctrl: NonNullable<ReturnType<typeof useController>["data"]>;
  cluster: string;
  ns: string;
  name: string;
  canUpdate?: boolean;
}) {
  const { toast } = useToast();
  const queryClient = useQueryClient();
  const [forms, setForms] = useState(() => emptyProbesForm(ctrl.probes));
  const [saving, setSaving] = useState(false);
  const [conflicts, setConflicts] = useState<import("../api/client").ConflictInfo[]>([]);
  const [showConflict, setShowConflict] = useState(false);

  const handleSave = async (force = false) => {
    setSaving(true);
    try {
      const probes = buildProbesSpec(forms);
      await updateController(cluster, name, ns, {
        spec: {
          ...(probes ? { probes } : {}),
        },
      }, { force });
      queryClient.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      setConflicts([]);
      setShowConflict(false);
      toast("Health probes updated");
    } catch (e) {
      if (e instanceof ControllerConflictError) {
        setConflicts(e.conflicts);
        setShowConflict(true);
      } else {
        toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
      }
    }
    setSaving(false);
  };

  if (!canUpdate) {
    return (
      <Card title="Health probes">
        <KVGrid
          items={PROBE_KINDS.map((kind) => {
            const probe = ctrl.probes?.[kind];
            const defaults = PROBE_DEFAULTS[kind];
            return {
              key: titleCaseProbe(kind),
              value: probe?.disabled
                ? "Disabled"
                : `Enabled · delay ${probe?.initialDelaySeconds ?? defaults.initialDelaySeconds}s · every ${probe?.periodSeconds ?? defaults.periodSeconds}s · timeout ${probe?.timeoutSeconds ?? defaults.timeoutSeconds}s`,
            };
          })}
        />
      </Card>
    );
  }

  return (
    <>
    <Card title="Health probes" headerRight={<span className={styles.cardNote}>Saving rolls the pod</span>}>
      <details open>
        <summary style={{ cursor: "pointer", fontWeight: 700 }}>Configure probe timings</summary>
        <div className={styles.muted} style={{ marginTop: 4, marginBottom: 12 }}>
          Jenkins container probes. Leave blank to keep the backend defaults.
        </div>
        {PROBE_KINDS.map((kind) => (
          <ProbePanel
            key={kind}
            kind={kind}
            form={forms[kind]}
            onChange={(next) => setForms((prev) => ({ ...prev, [kind]: next }))}
          />
        ))}
      </details>
      {canUpdate && <div style={{ marginTop: 10 }}>
        <Button size="sm" variant="primary" disabled={saving} onClick={() => handleSave()}>
          {saving ? "Saving..." : "Save probes"}
        </Button>
      </div>}
    </Card>
    <ConflictDialog
      conflicts={conflicts}
      open={showConflict}
      onReload={() => {
        setShowConflict(false);
        setConflicts([]);
        queryClient.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      }}
      onOverride={() => handleSave(true)}
    />
    </>
  );
}

// ---- Hibernation settings card ----

function HibernationCard({
  ctrl,
  cluster,
  ns,
  name,
  canUpdate,
}: {
  ctrl: NonNullable<ReturnType<typeof useController>["data"]>;
  cluster: string;
  ns: string;
  name: string;
  canUpdate?: boolean;
}) {
  const { toast } = useToast();
  const queryClient = useQueryClient();
  const [enabled, setEnabled] = useState(
    (ctrl as any).hibernation?.enabled ?? false
  );
  const [gracePeriodMinutes, setGracePeriodMinutes] = useState(
    (ctrl as any).hibernation?.gracePeriodMinutes ?? 60
  );
  const [activityIgnoreRegex, setActivityIgnoreRegex] = useState(
    (ctrl as any).hibernation?.activityIgnoreRegex ?? ""
  );
  const [saving, setSaving] = useState(false);
  const [conflicts, setConflicts] = useState<import("../api/client").ConflictInfo[]>([]);
  const [showConflict, setShowConflict] = useState(false);

  const handleSave = async (force = false) => {
    setSaving(true);
    try {
      const patch: Record<string, unknown> = {
        spec: {
          hibernation: {
            enabled,
            gracePeriodMinutes: Number(gracePeriodMinutes),
          },
        },
      };
      if (activityIgnoreRegex.trim()) {
        (patch.spec as Record<string, any>).hibernation.activityIgnoreRegex = activityIgnoreRegex.trim();
      } else {
        (patch.spec as Record<string, any>).hibernation.activityIgnoreRegex = null;
      }
      await updateController(cluster, name, ns, patch, { force });
      queryClient.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      setConflicts([]);
      setShowConflict(false);
      toast("Hibernation settings updated");
    } catch (e) {
      if (e instanceof ControllerConflictError) {
        setConflicts(e.conflicts);
        setShowConflict(true);
      } else {
        toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
      }
    }
    setSaving(false);
  };

  return (
    <>
    <Card title="Hibernation" headerRight={<span className={styles.cardNote}>Saving rolls the pod</span>}>
      <div className={styles.statLine}>
        <span className={styles.sk}>Enabled</span>
        <span>{enabled ? "Yes" : "No"}</span>
      </div>
      <div className={styles.statLine}>
        <span className={styles.sk}>Grace period (min)</span>
        <span>{gracePeriodMinutes}</span>
      </div>
      <div className={styles.statLine}>
        <span className={styles.sk}>Activity ignore regex</span>
      </div>
      <div className={styles.muted} style={{ fontSize: 12 }}>
        {activityIgnoreRegex || "(none)"}
      </div>
      {canUpdate && <>
        <div className={styles.statLine}>
          <span className={styles.sk}>Enabled</span>
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
        </div>
        <div className={styles.statLine}>
          <span className={styles.sk}>Grace period (min)</span>
          <input
            type="number"
            min={5}
            value={gracePeriodMinutes}
            onChange={(e) => setGracePeriodMinutes(e.target.value)}
            className={styles.policyInput}
          />
        </div>
        <div className={styles.statLine}>
          <span className={styles.sk}>Activity ignore regex</span>
        </div>
        <input
          type="text"
          value={activityIgnoreRegex}
          onChange={(e) => setActivityIgnoreRegex(e.target.value)}
          placeholder="/path/to/ignore.*"
          className={styles.policyInput}
          style={{ width: "100%", marginTop: 4 }}
        />
        <div className={styles.muted} style={{ marginTop: 4, fontSize: 11 }}>
          Path regex for requests to exclude from activity tracking. Changing this rolls the pod.
        </div>
        <div style={{ marginTop: 10 }}>
          <Button size="sm" variant="primary" disabled={saving} onClick={() => handleSave()}>
            {saving ? "Saving..." : "Save hibernation settings"}
          </Button>
        </div>
      </>}
    </Card>
    <ConflictDialog
      conflicts={conflicts}
      open={showConflict}
      onReload={() => {
        setShowConflict(false);
        setConflicts([]);
        queryClient.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      }}
      onOverride={() => handleSave(true)}
    />
    </>
  );
}

// VersionRollBanner renders the async roll/hold/block surface from the A/B
// conditions. upgradeBlocked (B) wins over held (A); in-progress shows no banner
// (it renders as an inline chip on VersionCard).
export function VersionRollBanner({
  versionStatus,
}: {
  versionStatus: NonNullable<ReturnType<typeof useController>["data"]>["versionStatus"];
}) {
  const vs = versionStatus;
  if (!vs) return null;

  if (vs.upgradeBlocked === true) {
    return (
      <div className={styles.versionBannerBlocked}>
        <div className={styles.pendingIcon}>⛔</div>
        <div className={styles.pendingBody}>
          <div className={styles.pendingTitle}>Upgrade blocked: {vs.blockedMessage || "core incompatible with plugin set"}</div>
          {vs.blockedReason && <div className={styles.pendingMeta}>Reason: {vs.blockedReason}</div>}
        </div>
      </div>
    );
  }
  if (vs.rollPending === true && vs.rollReason !== "VersionRollStarted") {
    return (
      <div className={styles.versionBannerHeld}>
        <div className={styles.pendingIcon}>⚠</div>
        <div className={styles.pendingBody}>
          <div className={styles.pendingTitle}>Upgrade held: {vs.rollMessage || "version roll is on hold"}</div>
          {vs.rollReason && <div className={styles.pendingMeta}>Reason: {vs.rollReason}</div>}
        </div>
      </div>
    );
  }
  return null;
}

// ReconcileBlockedBanner surfaces ConditionReconcileBlocked=True (C3). It is
// an above-the-fold alert that outranks the generic Conditions card, mirroring
// the convention established by VersionRollBanner.
export function ReconcileBlockedBanner({
  reconcileBlocked,
}: {
  // Optional at the component boundary: the detail contract always populates
  // this, but the banner stays defensive so it can be rendered in isolation.
  reconcileBlocked?: NonNullable<ReturnType<typeof useController>["data"]>["reconcileBlocked"];
}) {
  if (!reconcileBlocked || !reconcileBlocked.blocked) return null;

  return (
    <div className={styles.versionBannerBlocked}>
      <div className={styles.pendingIcon}>🚫</div>
      <div className={styles.pendingBody}>
        <div className={styles.pendingTitle}>Reconcile blocked: {reconcileBlocked.message || "a reconcile error is blocking progress"}</div>
        {reconcileBlocked.reason && <div className={styles.pendingMeta}>Reason: {reconcileBlocked.reason}</div>}
        {reconcileBlocked.since && <div className={styles.pendingMeta}>Since: {reconcileBlocked.since}</div>}
      </div>
    </div>
  );
}

// PluginConflictBanner surfaces ConditionPluginConflict=True (C4). It is an
// above-the-fold alert for when a plugin pin (catalog item, spec.pluginSpec,
// or bundle git/OCI input) no longer matches the active JenkinsVersionProfile
// / core lock.
export function PluginConflictBanner({
  pluginConflict,
}: {
  pluginConflict: NonNullable<ReturnType<typeof useController>["data"]>["pluginConflict"];
}) {
  if (!pluginConflict || !pluginConflict.active) return null;

  return (
    <div className={styles.versionBannerBlocked}>
      <div className={styles.pendingIcon}>🔒</div>
      <div className={styles.pendingBody}>
        <div className={styles.pendingTitle}>Plugin lock conflict: {pluginConflict.message || "a pin no longer matches the active core lock"}</div>
        {pluginConflict.reason && <div className={styles.pendingMeta}>Reason: {pluginConflict.reason}</div>}
      </div>
    </div>
  );
}

function PendingRestartBanner({
  pendingRestart,
  cluster,
  namespace,
  name,
  kind,
}: {
  pendingRestart: NonNullable<ReturnType<typeof useController>["data"]>["pendingRestart"];
  cluster: string;
  namespace: string;
  name: string;
  kind: "drift" | "plugin";
}) {
  const { toast } = useToast();
  const [approving, setApproving] = useState<"reload" | "restart" | null>(null);
  const [showRestartConfirm, setShowRestartConfirm] = useState(false);
  const { permissions } = useAuth();
  const queryClient = useQueryClient();

  if (!pendingRestart) return null;

  const handleApprove = async (action: "reload" | "restart") => {
    setApproving(action);
    try {
      await approveRestart(cluster, namespace, name, action);
      toast(`Configuration ${action === "reload" ? "reload" : "restart"} approved`);
      queryClient.invalidateQueries({ queryKey: ["controller", cluster, namespace, name] });
      queryClient.invalidateQueries({ queryKey: ["controllers"] });
    } catch (e) {
      toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
    }
    setApproving(null);
    setShowRestartConfirm(false);
  };

  const isDrift = kind === "drift";
  const title = isDrift
    ? "Configuration changes detected"
    : "Plugins installed, restart required to load them";
  const meta = isDrift
    ? `Detected ${new Date(pendingRestart.detectedAt).toLocaleString()} · ${
        pendingRestart.changes?.length
          ? `Changed: ${pendingRestart.changes.join(", ")}`
          : "Config drift detected"
      }`
    : `Detected ${new Date(pendingRestart.detectedAt).toLocaleString()} · Plugins installed, restart pending`;

  return (
    <div className={styles.pendingBanner}>
      <div className={styles.pendingIcon}>⚠</div>
      <div className={styles.pendingBody}>
        <div className={styles.pendingTitle}>{title}</div>
        <div className={styles.pendingMeta}>{meta}</div>
      </div>
      {canDoInNamespace(permissions, namespace, "controllers", "approve-restart") && (
        <div className={styles.pendingActions}>
        {isDrift && (
          <Button
            size="sm"
            variant="primary"
            disabled={approving !== null}
            onClick={() => handleApprove("reload")}
          >
            {approving === "reload" ? "⟳ Reloading..." : "↻ Reload Configuration"}
          </Button>
        )}
        {!showRestartConfirm ? (
          <Button
            size="sm"
            variant={isDrift ? "ghost" : "primary"}
            disabled={approving !== null}
            onClick={() => setShowRestartConfirm(true)}
          >
            ⚡ Safe Restart
          </Button>
        ) : (
          <div className={styles.restartConfirm}>
            <span className={styles.muted}>Restart Jenkins?</span>
            <Button
              size="sm"
              variant="primary"
              disabled={approving !== null}
              onClick={() => handleApprove("restart")}
            >
              {approving === "restart" ? "⟳ Restarting..." : "Yes, restart"}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setShowRestartConfirm(false)}>
              Cancel
            </Button>
          </div>
        )}
      </div>
      )}
    </div>
  );
}

// --- Pending Deletion Banner ---

function PendingDeletionBanner({
  pendingDeletions,
  cluster,
  namespace,
  name,
}: {
  pendingDeletions: NonNullable<ReturnType<typeof useController>["data"]>["pendingItemDeletions"];
  cluster: string;
  namespace: string;
  name: string;
}) {
  const { toast } = useToast();
  const [approving, setApproving] = useState<string | null>(null);
  const { permissions } = useAuth();
  const queryClient = useQueryClient();

  if (!pendingDeletions || pendingDeletions.length === 0) return null;

  const handleApprove = async (path: string) => {
    setApproving(path);
    try {
      await approveDeletion(cluster, namespace, name, path);
      toast(`Item deletion of ${path} approved`);
      queryClient.invalidateQueries({ queryKey: ["controller", cluster, namespace, name] });
      queryClient.invalidateQueries({ queryKey: ["controllers"] });
    } catch (e) {
      toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
    }
    setApproving(null);
  };

  return (
    <div className={styles.pendingBanner}>
      <div className={styles.pendingIcon}>⚠</div>
      <div className={styles.pendingBody}>
        <div className={styles.pendingTitle}>Pending item deletion(s)</div>
        <div className={styles.pendingMeta}>
          {pendingDeletions.length} item deletion(s) deferred because a build is running.
          Approving will permanently delete the item and its build history.
        </div>
        {pendingDeletions.map((d) => (
          <div key={d.path} className={styles.deferredItem}>
            <span className={styles.muted}>{d.path}</span>
            <span className={styles.muted}> — {d.reason}</span>
            {canDoInNamespace(permissions, namespace, "controllers", "approve-deletion") && (
              <Button
                size="sm"
                variant="primary"
                disabled={approving !== null}
                onClick={() => handleApprove(d.path)}
                className={styles.deferredApprove}
              >
                {approving === d.path ? "⟳ Approving..." : "Approve deletion"}
              </Button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

/* ── VerdictStrip (5.2) ── */

function VerdictStrip({
  verdict,
  ctrl,
  cluster,
  namespace,
  name: _name,
  canUpdate,
  attachedBundle,
  detachMutation,
  reloadConfig,
  reconcile,
  reconcilePending,
  setActiveTab,
}: {
  verdict: { tone: "ok" | "warn" | "bad"; head: string; sub?: string; icon: string };
  ctrl: ControllerData;
  cluster: string;
  namespace: string;
  name: string;
  canUpdate: boolean;
  attachedBundle: any;
  detachMutation: { mutate: () => void; isPending: boolean };
  reloadConfig: { mutate: () => void; isPending: boolean };
  reconcile: () => Promise<void>;
  reconcilePending: boolean;
  setActiveTab: (tab: string) => void;
}) { void _name;
  return (
    <div className={styles.verdictStrip} data-od-id="verdict-strip" data-tone={verdict.tone}>
      <div className={styles.verdictIcon} data-tone={verdict.tone}>{verdict.icon}</div>
      <div className={styles.verdictBody}>
        <div className={styles.verdictHead}>{verdict.head}</div>
        {verdict.sub && <div className={styles.verdictSub}>{verdict.sub}</div>}
      </div>
      <div className={styles.verdictActions}>
        {!ctrl.miteConnected && (
          <>
            <Button size="sm" variant="primary" onClick={reconcile} disabled={reconcilePending}>
              Reconcile
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setActiveTab("diagnostics")}>
              Diagnose
            </Button>
          </>
        )}
        {ctrl.miteConnected && ctrl.lastApplyResult?.succeeded === false && (
          <>
            <Button size="sm" variant="ghost" onClick={() => setActiveTab("configuration")}>
              View diff
            </Button>
            <Button size="sm" variant="ghost" onClick={() => reloadConfig.mutate()} disabled={reloadConfig.isPending}>
              Retry apply
            </Button>
          </>
        )}
        {ctrl.miteConnected && (ctrl.lastApplyResult?.succeeded !== false) && attachedBundle?.status?.phase === "Invalid" && (
          <>
            <Link
              to={`/catalog/bundles/${encodeURIComponent(attachedBundle.metadata.namespace ?? namespace)}/${encodeURIComponent(attachedBundle.metadata.name)}${clusterQuery(cluster)}`}
            >
              <Button size="sm" variant="primary">Fix bundle</Button>
            </Link>
            {canUpdate && (
              <Button size="sm" variant="ghost" onClick={() => detachMutation.mutate()} disabled={detachMutation.isPending}>
                Detach
              </Button>
            )}
          </>
        )}
        {ctrl.miteConnected && (ctrl.lastApplyResult?.succeeded !== false) && !(attachedBundle?.status?.phase === "Invalid") && ctrl.liveDrift?.detected && (
          <>
            <Button size="sm" variant="ghost" onClick={() => setActiveTab("configuration")}>
              View diff
            </Button>
            <Button size="sm" variant="primary" onClick={reconcile} disabled={reconcilePending}>
              Reconcile
            </Button>
          </>
        )}
      </div>
    </div>
  );
}

// --- Reconciliation Policy Edit Form ---

function PolicyEditForm({
  policy,
  cluster,
  namespace,
  name,
  onDone,
}: {
  policy?: ReconciliationPolicy;
  cluster: string;
  namespace: string;
  name: string;
  onDone: () => void;
}) {
  const { toast } = useToast();
  const [mode, setMode] = useState<ReconciliationMode>(policy?.mode || "automatic");
  const [interval, setInterval] = useState(policy?.interval || "");
  const [maxDeferSeconds, setMaxDeferSeconds] = useState(policy?.maxDeferSeconds || 0);
  const [drainTimeoutSeconds, setDrainTimeoutSeconds] = useState(policy?.drainTimeoutSeconds || 0);
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    setSaving(true);
    try {
      // Send a merge patch — only the fields we're changing.
      const patch = {
        spec: {
          reconciliationPolicy: {
            mode,
            interval: interval || null,
            maxDeferSeconds: maxDeferSeconds || null,
            drainTimeoutSeconds: drainTimeoutSeconds || null,
          },
        },
      };
      await updateController(cluster, name, namespace, patch);
      toast("Reconciliation policy updated");
      onDone();
    } catch (e) {
      toast(`Failed: ${e instanceof Error ? e.message : "unknown"}`);
    }
    setSaving(false);
  };

  return (
    <div>
      <div className={styles.statLine}>
        <span className={styles.sk}>Mode</span>
        <select
          value={mode}
          onChange={(e) => setMode(e.target.value as ReconciliationMode)}
          className={styles.policySelect}
        >
          <option value="automatic">Automatic — push on config drift</option>
          <option value="idle">Idle — defer while builds running</option>
          <option value="manual">Manual — require approval</option>
        </select>
      </div>
      <div className={styles.statLine}>
        <span className={styles.sk}>Interval</span>
        <input
          type="text"
          value={interval}
          onChange={(e) => setInterval(e.target.value)}
          placeholder="30s (default)"
          className={styles.policyInput}
        />
      </div>
      <div className={styles.statLine}>
        <span className={styles.sk} />
        <span className={styles.muted}>e.g. 30s, 5m, 1h (min 10s)</span>
      </div>
      {mode === "idle" && (
        <>
          <div className={styles.statLine}>
            <span className={styles.sk}>Max defer</span>
            <input
              type="number"
              value={maxDeferSeconds}
              onChange={(e) => setMaxDeferSeconds(Number(e.target.value))}
              placeholder="1800 (default)"
              className={styles.policyInput}
            />
          </div>
          <div className={styles.statLine}>
            <span className={styles.sk} />
            <span className={styles.muted}>seconds before apply proceeds regardless (default 1800)</span>
          </div>
        </>
      )}
      <div className={styles.statLine}>
        <span className={styles.sk}>Drain timeout</span>
        <input
          type="number"
          value={drainTimeoutSeconds}
          onChange={(e) => setDrainTimeoutSeconds(Number(e.target.value))}
          placeholder="900 (default)"
          className={styles.policyInput}
        />
      </div>
      <div className={styles.statLine}>
        <span className={styles.sk} />
        <span className={styles.muted}>seconds to drain builds before restart (0 = immediate)</span>
      </div>
      <div style={{ marginTop: 8 }}>
        <Button size="sm" variant="primary" disabled={saving} onClick={handleSave}>
          {saving ? "Saving..." : "Save Policy"}
        </Button>
        <Button size="sm" variant="ghost" onClick={onDone}>
          Cancel
        </Button>
      </div>
    </div>
  );
}

function ReconcileButton({ onReconcile, pending }: { onReconcile: () => Promise<void>; pending: boolean }) {
  return (
    <Button size="sm" onClick={onReconcile} disabled={pending}>
      {pending ? "⟳ Reconciling..." : "⟳ Reconcile"}
    </Button>
  );
}
