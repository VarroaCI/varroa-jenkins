import { useState, useEffect, useCallback, useRef, useMemo } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useController } from "../hooks/useControllers";
import type { PluginInventoryDriftEntry, ControllerDetail } from "../hooks/useControllers";
import { useClusters, coreOf } from "../hooks/useClusters";
import { clusterQuery, controllerRoute } from "../routing";
import { useEventStream } from "../hooks/useEventStream";
import { ApiError, bffFetch } from "../hooks/useApi";
import { ForbiddenPage, GenericErrorPage, NotFoundPage } from "../components/RecoveryState";
import { useComposedBundles } from "../hooks/useCatalog";
import { useToast } from "../components/Toast";
import { BFF_BASE, updateController, ControllerConflictError, approveRestart, reprovisionController, restartController, setPowerState, hibernateController, wakeController, approveDeletion, getProvisioningConfig, getVersionProfiles, parsePreflightChecks } from "../api/client";
import VersionPicker from "../components/VersionPicker";
import { overlayJenkinsImage } from "../lib/overlay";
import { upgradeInfo, versionsDiffer } from "../lib/versionCatalog";
import { pluginDiff } from "../lib/pluginDiff";
import { unappliedRemovalNotice } from "../lib/unappliedRemovals";
import { Card } from "../components/Card";
import { Tabs } from "../components/Tabs";
import { age } from "../components/activityTimeline.util";
import { KVGrid } from "../components/KVGrid";
import { Button } from "../components/Button";
import { BundleSelector, BundleHealthBadge } from "../components/BundleSelector";
import { DeliveryPipeline, isAsleep } from "../components/DeliveryPipeline";
import { ATTENTION_LABEL } from "../components/StatusPill";
import { useControllerDiff } from "../hooks/useControllerDiff";
import { useActivityFeed } from "../hooks/useActivityFeed";
import { useAuth } from "../context/AuthContext";
import { canDoInNamespace } from "../hooks/usePermissions";
import type {
  ReconciliationPolicy,
  ReconciliationMode,
  PreflightCheck,
  ProbeSpec,
  ProbesSpec,
  HibernationSpec,
  ControllerAttention,
  ControllerAttentionKind,
} from "../types";
import { PROBE_DEFAULTS } from "../types";
import ConflictDialog from "../components/ConflictDialog";
import SpecEditorCard from "../components/specform/SpecEditorCard";
import styles from "./ControllerDetail.module.css";

const TABS = [
  { id: "overview", label: <>▦ Overview</> },
  { id: "configuration", label: <>⚯ Configuration</> },
  { id: "diagnostics", label: <>◷ Diagnostics</> },
];
const VALID_TABS = new Set(TABS.map((t) => t.id));

type ControllerData = NonNullable<ReturnType<typeof useController>["data"]>;

// The backend serializes certExpiry as a date-only string ("2006-01-02", see
// internal/api/handlers.go). `new Date("2026-08-20")` parses that as UTC
// midnight, so toLocaleDateString() would render the *previous* calendar day
// in any timezone west of UTC. Split the components and build a local
// calendar date instead. Full timestamps and malformed inputs fall back to
// plain Date parsing / the raw string.
function fmtCertDate(dateStr: string): string {
  const [y, m, d] = dateStr.split("-").map(Number);
  if (y && m && d) return new Date(y, m - 1, d).toLocaleDateString();
  const parsed = new Date(dateStr);
  return isNaN(parsed.getTime()) ? dateStr : parsed.toLocaleDateString();
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

  // Live activity feed for this controller, shared by the header phase pill
  // (newest phase change), the Overview feed, and the Diagnostics stream.
  const feed = useActivityFeed({ cluster, namespace, name });
  const events = feed.events;
  const phaseChangeTs = useMemo(() => {
    const phaseEvents = events.filter((e) => e.type === "phase");
    return phaseEvents.length > 0 ? phaseEvents[0].timestamp : undefined;
  }, [events]);

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
    mutationFn: (state: "Running" | "Stopped") => setPowerState(cluster, name, namespace, state),
    onSuccess: () => { toast("Power state updated"); invalidateAll(); },
    onError: (e) => toast(`Power state change failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const hibernate = useMutation({
    mutationFn: () => hibernateController(cluster, name, namespace),
    onSuccess: () => { toast("Hibernation triggered"); invalidateAll(); },
    onError: (e) => toast(`Hibernate failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const wake = useMutation({
    mutationFn: () => wakeController(cluster, name, namespace),
    onSuccess: () => { toast("Wake triggered"); invalidateAll(); },
    onError: (e) => toast(`Wake failed: ${e instanceof Error ? e.message : "unknown"}`),
  });

  const reloadConfig = useMutation({
    mutationFn: () => approveRestart(cluster, namespace, name, "reload"),
    onSuccess: () => { toast("Reload triggered"); invalidateAll(); },
    onError: (e) => toast(`Reload failed: ${e instanceof Error ? e.message : "unknown"}`),
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

  // Stale persisted tab values ("plugins", "observability") fall back to
  // overview instead of rendering a blank content area.
  const active = VALID_TABS.has(activeTab) ? activeTab : "overview";

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
    : confirm === "power-off" ? power.isPending
    : confirm === "hibernate" ? hibernate.isPending
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
        wake={wake}
        phaseChangeTs={phaseChangeTs}
      />

      <VersionRollBanner versionStatus={ctrl.versionStatus} />
      <ReconcileBlockedBanner reconcileBlocked={ctrl.reconcileBlocked} />
      <AttentionBanner attention={ctrl.attention} />
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

      {confirm && (
        <div className={styles.headerConfirm} role="alertdialog" aria-label={`Confirm ${confirm}`}>
          <span>{confirmBody[confirm]}</span>
          <span className={styles.muted}>{runningBuildsLine}</span>
          <div className={styles.confirmActions}>
            <Button size="sm" variant="primary" disabled={confirmPending} onClick={() => {
              const onSettled = () => setConfirm(null);
              if (confirm === "restart") restart.mutate(undefined, { onSettled });
              else if (confirm === "power-off") power.mutate("Stopped", { onSettled });
              else if (confirm === "hibernate") hibernate.mutate(undefined, { onSettled });
              else if (confirm === "reprovision") reprovision.mutate(undefined, { onSettled });
            }}>
              {confirmPending ? confirmProgressLabel[confirm] : `Yes, ${confirm === "power-off" ? "power off" : confirm}`}
            </Button>
            <Button size="sm" variant="ghost" disabled={confirmPending} onClick={() => setConfirm(null)}>Cancel</Button>
          </div>
        </div>
      )}

      <Tabs tabs={TABS} activeTab={active} onSelect={setActiveTab} />

      {active === "overview" && (
        <OverviewTab
          ctrl={ctrl}
          cluster={cluster}
          namespace={namespace}
          name={name}
          canUpdate={canUpdate}
          events={events}
          onShowHistory={() => setActiveTab("diagnostics")}
        />
      )}
      {active === "configuration" && (
        <ConfigurationTab
          ctrl={ctrl}
          cluster={cluster}
          namespace={namespace}
          name={name}
          canUpdate={canUpdate}
        />
      )}
      {active === "diagnostics" && (
        <DiagnosticsTab
          ctrl={ctrl}
          cluster={cluster}
          namespace={namespace}
          name={name}
          isCore={isCore}
          events={events}
          activityReady={feed.readyState}
        />
      )}
    </div>
  );
}

type PillTone = "ok" | "warn" | "bad" | "asleep";

// Phase pill: tone per state (task #483). "Applying" and "Apply failed" are
// derived from apply state while the controller is Connected.
function phasePill(ctrl: ControllerData): { label: string; tone: PillTone } {
  const p = ctrl.phase || "Pending";
  if (p === "Hibernated" || p === "Stopped") return { label: p, tone: "asleep" };
  // An asleep controller shows its asleep state; the BFF never sets attention
  // for those phases.
  if (ctrl.attention) return { label: ATTENTION_LABEL[ctrl.attention.kind], tone: "bad" };
  // A failed apply is the concrete outcome in every phase, not only Connected.
  if (ctrl.lastApplyResult?.succeeded === false) return { label: "Apply failed", tone: "bad" };
  if (p === "Connected") {
    const hashMismatch =
      !!ctrl.desiredStateHash && !!ctrl.lastApplyResult?.hash && ctrl.lastApplyResult.hash !== ctrl.desiredStateHash;
    // Manual mode: a hash mismatch means the change is parked awaiting operator
    // approval, not actively being applied.
    if (hashMismatch && ctrl.reconciliationPolicy?.mode === "manual") return { label: "Awaiting approval", tone: "warn" };
    if (hashMismatch) return { label: "Applying", tone: "warn" };
    return { label: "Connected", tone: "ok" };
  }
  if (p === "Failed") return { label: "Failed", tone: "bad" };
  return { label: p, tone: "warn" };
}

function endpointHost(endpoint?: string): string | null {
  if (!endpoint) return null;
  try { return new URL(endpoint).host; } catch { return endpoint; }
}

function ControllerHeader({
  ctrl, cluster, namespace, name, isCore,
  canManage, reloadConfig, reconcile, reconcilePending, setConfirm, power, wake, phaseChangeTs,
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
  power: { mutate: (state: "Running" | "Stopped") => void; isPending: boolean };
  wake: { mutate: () => void; isPending: boolean };
  phaseChangeTs?: string;
}) {
  const stopped = ctrl.powerState === "Stopped" || ctrl.phase === "Stopped";
  const hibernated = ctrl.hibernated === true || ctrl.phase === "Hibernated";
  const asleep = stopped || hibernated;
  const pill = phasePill(ctrl);
  const host = endpointHost(ctrl.endpoint);
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

  return <header className={styles.detailHero}>
    <div className={styles.ctrlIc}>⬡</div>
    <div className={styles.heroMeta}>
      <h1>
        {ctrl.name}{" "}
        <span className={`${styles.phase} ${styles[pill.tone]}`}>
          <span className={styles.lamp} />
          {pill.label}
        </span>
        {phaseChangeTs && (
          <span className={styles.statechange}>
            for {age(phaseChangeTs).replace(/\s*ago$/, "")}
          </span>
        )}
      </h1>
      <div className={styles.path}>
        <span>{cluster} / {namespace} · </span>
        {host ? (
          <a href={ctrl.endpoint} target="_blank" rel="noreferrer">{host}</a>
        ) : (
          <span>no endpoint</span>
        )}
      </div>
    </div>
    <div className={styles.heroActions}>
      {asleep && (
        <Button size="sm" variant="primary" disabled={power.isPending || wake.isPending} onClick={() => (hibernated ? wake.mutate() : power.mutate("Running"))}>
          {power.isPending || wake.isPending ? "Waking…" : hibernated ? "Wake" : "Power On"}
        </Button>
      )}
      {!asleep && (ctrl.endpoint ? (
        <a href={ctrl.endpoint} target="_blank" rel="noreferrer">
          <Button variant="primary" size="sm">Open Jenkins</Button>
        </a>
      ) : (
        <Button size="sm" disabled>Not ready</Button>
      ))}
      {!asleep && isCore && ctrl.routingMode === "path" && (
        <Link to={`${controllerRoute(cluster, namespace, name)}/jenkins`}>
          <Button variant="ghost" size="sm">View embedded</Button>
        </Link>
      )}
      {!asleep && <ReconcileButton onReconcile={reconcile} pending={reconcilePending} />}
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
  cluster,
  namespace,
  name,
  canUpdate,
  events,
  onShowHistory,
}: {
  ctrl: NonNullable<ReturnType<typeof useController>["data"]>;
  cluster: string;
  namespace: string;
  name: string;
  canUpdate: boolean;
  events: import("../types").ActivityEvent[];
  onShowHistory: () => void;
}) {
  const { diff, loading: diffLoading, error: diffError, fetchDiff } = useControllerDiff(cluster, namespace, name);
  return (
    <div className={styles.overview} data-testid="overview-tab">
      <DeliveryPipeline
        ctrl={ctrl}
        diff={diff}
        diffLoading={diffLoading}
        diffError={diffError}
        onFetchDiff={fetchDiff}
      />
      {!isAsleep(ctrl) && <VitalsLine ctrl={ctrl} />}
      {!isAsleep(ctrl) && <OverviewFeed events={events} onShowHistory={onShowHistory} />}
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

// One-line vitals under the pipeline (mockup `.vitals`). When the mite is
// offline and the controller is not hibernated, stale numbers are replaced
// with an explicit offline note.
//
// The observability summary the numbers come from is cached server-side; if
// that cache is stale or carries warnings, they are surfaced here rather than
// silently swallowed (the old standalone Observability tab rendered them).
function VitalsLine({ ctrl }: { ctrl: ControllerData }) {
  const offline = !ctrl.miteConnected;
  const jobs = ctrl.observability?.summary?.totalJobs ?? "—";
  const rbs = ctrl.observability?.summary?.runningBuilds ?? 0;
  const miteImage = ctrl.miteImageStatus;
  const obsFreshness = ctrl.observability?.freshness;
  const warnings = ctrl.observability?.warnings ?? [];
  // Per-source failure reasons can live only on ObservabilitySource.error
  // (independent of the top-level warnings array) — e.g. a source marked
  // `unavailable` with its reason stored on the source itself. Surface those
  // too so a degraded/unavailable source never renders silently.
  const sourceErrors =
    ctrl.observability?.sources?.filter(
      (s) => (s.status === "degraded" || s.status === "unavailable") && !!s.error
    ) ?? [];
  return (
    <>
      <div className={styles.vitals}>
        {offline ? (
          <span className={styles.metricsOffline}>metrics unavailable — mite offline</span>
        ) : (
          <>
            <span><b>{jobs}</b> jobs</span>
            <span className={styles.dot}>·</span>
            <span><b>{rbs}</b> running builds</span>
            <span className={styles.dot}>·</span>
            <span>{rbs > 0 ? "in flight" : "idle"}</span>
          </>
        )}
        <span className={styles.push}>
          {ctrl.certExpiry && (
            <span>cert expires {fmtCertDate(ctrl.certExpiry)} · </span>
          )}
          {miteImage && miteImage.stale !== undefined && (
            <span>image {miteImage.stale ? "stale" : "current"}</span>
          )}
          {obsFreshness && (obsFreshness.observedAt || obsFreshness.miteTTL) && (
            <span> · obs {obsFreshness.stale ? "stale" : "current"}</span>
          )}
        </span>
      </div>
      {(warnings.length > 0 || sourceErrors.length > 0) && (
        <div className={styles.obsWarnings}>
          {warnings.map((w, i) => (
            <span key={`warn-${i}`} className={styles.obsWarning}>⚠ {w.message}</span>
          ))}
          {sourceErrors.map((s) => (
            <span key={`src-${s.provider}`} className={styles.obsWarning}>⚠ {s.provider}: {s.error}</span>
          ))}
        </div>
      )}
    </>
  );
}

// The 5 most recent activity entries for this controller, with phase changes
// emphasised, and a link to the full Diagnostics stream.
function OverviewFeed({
  events,
  onShowHistory,
}: {
  events: import("../types").ActivityEvent[];
  onShowHistory: () => void;
}) {
  const recent = events.slice(0, 5);
  if (recent.length === 0) return null;
  return (
    <div className={styles.overviewFeed}>
      <div className={styles.overviewFeedHead}>
        <h2 className={styles.overviewFeedTitle}>Activity</h2>
        <button className={styles.more} onClick={onShowHistory}>Full history →</button>
      </div>
      <div className={styles.feed}>
        {recent.map((e, i) => (
          <div key={`${e.timestamp}-${i}`} className={`${styles.row} ${e.type === "phase" ? styles.phaseRow : ""}`}>
            <span className={styles.t}>{new Date(e.timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false })}</span>
            <span>{e.message}</span>
          </div>
        ))}
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
    onSuccess: (data: ControllerDetail) => {
      invalidate();
      // A detach sends composedBundleRef: null — an explicit removal that can
      // be retained server-side if another manager owns the field. Don't report
      // unqualified success when the server says the removal didn't apply.
      toast(unappliedRemovalNotice(data) ?? ok);
      setShowSelector(false);
      setSelectedBundle(null);
    },
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

function DiagnosticsTab({ ctrl, cluster, namespace, name, isCore, events, activityReady }: {
  ctrl: ControllerData; cluster: string; namespace: string; name: string; isCore: boolean;
  events: import("../types").ActivityEvent[]; activityReady: string;
}) {
  return (
    <div className={styles.diagnosticTabs} data-testid="diagnostics-tab">
      <div className={styles.diagGrid}>
        <ConnectionPanel ctrl={ctrl} />
        <ConditionsPanel ctrl={ctrl} />
      </div>
      <PluginDriftPanel ctrl={ctrl} />
      <ControllerStream
        cluster={cluster}
        namespace={namespace}
        name={name}
        isCore={isCore}
        events={events}
        activityReady={activityReady}
      />
      <ApplyHistoryPanel ctrl={ctrl} />
    </div>
  );
}

// ---- Connection panel ----

function ConnectionPanel({ ctrl }: { ctrl: ControllerData }) {
  const miteImage = ctrl.miteImageStatus;
  return (
    <Card title="Connection">
      <dl className={styles.kv}>
        <dt>mite</dt>
        <dd>{ctrl.miteVersion || "—"}{miteImage && miteImage.stale !== undefined ? ` · image ${miteImage.stale ? "stale" : "current"}` : ""}</dd>
        <dt>heartbeat</dt>
        <dd>{ctrl.lastSeen ? age(ctrl.lastSeen, { variant: "heartbeat" }) : "—"}</dd>
        <dt>certificate</dt>
        <dd>{ctrl.certExpiry ? `expires ${fmtCertDate(ctrl.certExpiry)}` : "—"}</dd>
        <dt>gateway</dt>
        <dd>grpc mTLS · stream {ctrl.miteConnected ? "open" : "closed"}</dd>
      </dl>
    </Card>
  );
}

// ---- Conditions panel ----

function ConditionsPanel({ ctrl }: { ctrl: ControllerData }) {
  const conditions = ctrl.conditions ?? [];
  return (
    <Card title="Conditions">
      {conditions.length > 0 ? (
        <div className={styles.conditions}>
          {conditions.map((condition) => (
            <div className={styles.condItem} key={condition.type}>
              <div className={`${styles.condDot} ${condition.status === "True" ? styles.softOk : styles.softBad}`} />
              <div>
                <div className={styles.ft}><b>{condition.type}</b> · {condition.status}</div>
                <div className={styles.fm}>{condition.reason || condition.message || "—"}</div>
              </div>
            </div>
          ))}
        </div>
      ) : (
        <div className={styles.allclear}>
          <span className={styles.lamp} />
          All clear — nothing needs attention
        </div>
      )}
    </Card>
  );
}

// ---- Plugin drift panel (absorbed from the old Plugins tab) ----

const PLUGIN_QUALIFIER_TONE: Record<string, string> = {
  warn: styles.qualifierWarn,
  bad: styles.qualifierBad,
  neutral: styles.qualifierNeutral,
};

function driftReason(d: PluginInventoryDriftEntry): string {
  if (d.message) return d.message;
  if (d.class === "optional-dependency") return "optional dependency — pulled in transitively, not declared";
  if (d.class === "unmanaged") return "unmanaged — not in the bundle";
  return d.class ? `${d.class} — not tracked by the bundle` : "unmanaged — not in the bundle";
}

export function PluginDriftPanel({ ctrl }: { ctrl: ControllerData }) {
  const pi = ctrl.pluginInventory;
  const [expanded, setExpanded] = useState(false);

  if (!pi) {
    return (
      <Card title="Plugins">
        <p className={styles.pluginsEmpty}>No plugin inventory available yet — the mite has not reported one.</p>
      </Card>
    );
  }

  const declared = pi.counts?.declared;
  const unmanaged = pi.counts?.unmanaged;
  // versionDrift lists every entry whose version differs from the pin — the
  // "version drift" figure is the count of all drifted entries (ahead/behind/
  // missing verdicts alike), not just the "ahead" ones, so it is labelled
  // neutrally rather than as "ahead of pin".
  const driftCount = pi.versionDrift?.length ?? 0;
  // bootstrap / jenkins-supplied are secondary breakdowns of the inventory.
  // They stay out of the headline counts line and appear only inside the
  // expanded ("Show all") view.
  const bootstrap = pi.counts?.bootstrap;
  const jenkinsSupplied = pi.counts?.["jenkins-supplied"];

  const rows: { name: string; version: string; why: string }[] = [
    ...(pi.drift ?? []).map((d) => ({
      name: d.name,
      version: d.version || "",
      why: driftReason(d),
    })),
    ...(pi.versionDrift ?? []).map((d) => ({
      name: d.name,
      version: d.version || "",
      why: `${d.verdict || "version"} — pin differs`,
    })),
  ];

  const visible = expanded ? rows : rows.slice(0, 3);

  // Data-quality qualifiers: when the inventory is stale, degraded, or its
  // dependency closure / drift list was truncated, the drift readout is not
  // a trustworthy clean bill of health. Surface those flags instead of
  // presenting the result as unconditionally current.
  const qualifiers: Array<{ label: string; tone: "warn" | "bad" | "neutral" }> = [];
  if (pi.stale) qualifiers.push({ label: "stale — last synced data", tone: "warn" });
  if (pi.degraded) qualifiers.push({ label: "degraded", tone: "warn" });
  if (pi.bootstrapApproximate) qualifiers.push({ label: "approximate", tone: "neutral" });
  if (pi.optionalEdgesDropped) qualifiers.push({ label: "optional edges dropped", tone: "warn" });
  if (pi.truncated) qualifiers.push({ label: "truncated", tone: "bad" });
  if (pi.driftTruncated) qualifiers.push({ label: "drift list truncated", tone: "warn" });

  const driftIncomplete = pi.truncated || pi.optionalEdgesDropped || pi.driftTruncated;
  const untrustworthy = pi.stale || pi.degraded || driftIncomplete;
  // Each incompleteness flag has a distinct meaning — spell out exactly which
  // one(s) made the drift readout untrustworthy instead of collapsing them all
  // into a generic "(truncated)" message that misleads operators.
  const incompleteReasons: string[] = [];
  if (pi.truncated) incompleteReasons.push("the plugin inventory was truncated");
  if (pi.driftTruncated) incompleteReasons.push("the drift list was truncated");
  if (pi.optionalEdgesDropped) incompleteReasons.push("optional dependency edges were dropped from the collected graph");

  return (
    <Card
      title="Plugins"
      headerRight={
        qualifiers.length > 0 ? (
          <div className={styles.qualifiers}>
            {qualifiers.map((q) => (
              <span key={q.label} className={`${styles.qualifier} ${PLUGIN_QUALIFIER_TONE[q.tone]}`}>
                {q.label}
              </span>
            ))}
          </div>
        ) : undefined
      }
    >
      <div className={styles.counts}>
        <span><b>{declared ?? "—"}</b> declared</span>
        <span><b>{unmanaged ?? "—"}</b> unmanaged</span>
        <span><b>{driftCount}</b> version drift</span>
      </div>
      {expanded && (
        <div className={styles.counts}>
          <span><b>{bootstrap ?? "—"}</b> bootstrap</span>
          <span><b>{jenkinsSupplied ?? "—"}</b> jenkins-supplied</span>
        </div>
      )}
      {rows.length > 0 ? (
        <>
          {visible.map((r, i) => (
            <div key={`${r.name}-${i}`} className={styles.plugRow}>
              <span className={styles.mono}>{r.name}{r.version ? ` ${r.version}` : ""}</span>
              <span className={styles.why}>{r.why}</span>
            </div>
          ))}
        </>
      ) : untrustworthy ? (
        <p className={styles.muted}>
          No plugin drift shown — {driftIncomplete
            ? `${incompleteReasons.join("; ")}, so this is not a confirmed clean drift check.`
            : "the underlying data is stale or degraded and may not reflect the current bundle."}
        </p>
      ) : (
        <p className={styles.muted}>No plugin drift — everything matches the bundle.</p>
      )}
      {/* The toggle must appear whenever there is more to reveal: more than
          three rows, or secondary bootstrap/jenkins-supplied counts that stay
          hidden until the view is expanded. With few rows the secondary counts
          are the only reason to expand, so no row count goes in the label. */}
      {(rows.length > 3 || bootstrap !== undefined || jenkinsSupplied !== undefined) && (
        <button className={styles.expandBtn} onClick={() => setExpanded((e) => !e)}>
          {expanded ? "Show fewer" : rows.length > 3 ? `Show all ${rows.length} →` : "Show details"}
        </button>
      )}
    </Card>
  );
}

// ---- Merged stream (activity feed + pod logs) ----

const STREAM_CHIPS = ["All", "Operator", "Mite", "Jenkins", "User", "Logs"] as const;

interface LogEntry {
  timestamp?: string;
  level?: string;
  source?: string;
  message: string;
}

interface StreamEntry {
  key: string;
  ts: number;
  kind: "activity" | "log";
  timestamp: string;
  level?: string;
  source: string;
  phase?: boolean;
  message: string;
}

function ControllerStream({
  cluster, namespace, name, isCore, events, activityReady,
}: {
  cluster: string; namespace: string; name: string; isCore: boolean;
  events: import("../types").ActivityEvent[]; activityReady: string;
}) {
  const [chip, setChip] = useState<string>("All");
  const [lines, setLines] = useState<LogEntry[]>([]);
  // Dedupe replay bursts: the logs endpoint re-tails the last N lines on every
  // reconnect, so a burst of identical (source, message) lines arriving right
  // after an (re)connect is treated as a replay, not new log output.
  const recentRef = useRef<Array<{ source: string; message: string; at: number }>>([]);
  // Wall-clock time of the most recent readyState -> "open" transition. The
  // replay dedupe above only applies within a short settle window after that
  // transition; steady-state repeated lines are genuine output and must not be
  // dropped just because they repeat a very recent message.
  const connectedAtRef = useRef(0);

  const { lastEvent, readyState, error: streamError } = useEventStream<LogEntry>(
    isCore
      ? `${BFF_BASE}/clusters/${encodeURIComponent(cluster)}/controllers/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/logs?follow=true`
      : null,
    isCore ? `controller:${cluster}/${namespace}/${name}` : null,
  );

  useEffect(() => {
    if (readyState === "open") connectedAtRef.current = Date.now();
  }, [readyState]);

  useEffect(() => {
    if (lastEvent) {
      // The logs endpoint emits bare LogEntry frames (unlike the activity
      // stream's {type, data} wrapper); handle both shapes defensively.
      const raw = lastEvent as unknown;
      const wrapped = raw && typeof raw === "object" && "data" in raw
        ? (raw as { data: unknown }).data
        : raw;
      const entry = wrapped as LogEntry;
      if (entry && typeof entry.message === "string") {
        const now = Date.now();
        const source = entry.source ?? "";
        // Only the post-(re)connect settle window may contain replayed tail
        // lines; outside it, every line is new output even if it repeats one
        // seen very recently.
        const inSettleWindow = now - connectedAtRef.current < 5000;
        // A re-tailed line from the logs endpoint reports a timestamp from
        // BEFORE the (re)connect, while genuinely new output reports one at or
        // after it. Gate the replay dedupe on that too: a line that repeats a
        // recent (source, message) but is stamped at/after the connection is
        // new output (e.g. a heartbeat right after reconnect), not a replay.
        // If timestamp is missing/unparseable, treat it as new output rather
        // than risk dropping a real log line.
        const entryTs = entry.timestamp ? Date.parse(entry.timestamp) : NaN;
        const isPreConnectTail = !Number.isNaN(entryTs) && entryTs < connectedAtRef.current;
        const replay = inSettleWindow && isPreConnectTail && recentRef.current.some(
          (r) => r.source === source && r.message === entry.message,
        );
        if (!replay) {
          recentRef.current.push({ source, message: entry.message, at: now });
          if (recentRef.current.length > 200) recentRef.current = recentRef.current.slice(-150);
          setLines((prev) => {
            const next = [...prev, entry];
            return next.length > 500 ? next.slice(-500) : next;
          });
        }
      }
    }
  }, [lastEvent]);

  const entries = useMemo<StreamEntry[]>(() => {
    const onlyLogs = chip === "Logs";
    // "Logs" means "all log-kind entries regardless of source" (the !onlyLogs
    // guard above already excludes activity entries), so it must not apply a
    // source filter the way the Operator/Mite/Jenkins/User chips do.
    const chipSource = chip === "All" || onlyLogs ? "" : chip.toLowerCase();
    const out: StreamEntry[] = [];
    if (!onlyLogs) {
      for (const e of events) {
        const source = (e.source || "").toLowerCase();
        if (chipSource && source !== chipSource) continue;
        out.push({
          key: `a-${e.timestamp}-${e.type}-${source}-${e.message}`,
          ts: new Date(e.timestamp).getTime(),
          kind: "activity",
          timestamp: e.timestamp,
          source,
          phase: e.type === "phase",
          message: e.message,
        });
      }
    }
    lines.forEach((l, i) => {
      const source = (l.source || "").toLowerCase();
      if (chipSource && source !== chipSource) return;
      out.push({
        key: `l-${l.timestamp}-${i}-${l.message}`,
        ts: Date.parse(l.timestamp || "") || 0,
        kind: "log",
        timestamp: l.timestamp || "",
        level: l.level || "INFO",
        source,
        message: l.message,
      });
    });
    out.sort((a, b) => b.ts - a.ts);
    // Cap the merged feed at 500 entries, matching the documented browser
    // buffer (docs/operations/observability.md) and the 500-line cap applied
    // to `lines` above — not 400.
    return out.slice(0, 500);
  }, [events, lines, chip]);

  const logLive = readyState === "open";
  // With the "Logs" chip selected the feed shows only log lines, so liveness
  // should reflect the log SSE connection alone; a healthy activity stream
  // must not make a dead/stale log feed claim "live".
  const live = chip === "Logs" ? logLive : (logLive || activityReady === "open");

  return (
    <div className={styles.stream}>
      <div className={styles.bar}>
        <h4>Stream</h4>
        {STREAM_CHIPS.map((c) => {
          const disabled = c === "Logs" && !isCore;
          return (
            <button
              key={c}
              className={`${styles.fchip} ${chip === c ? styles.fchipOn : ""}`}
              aria-pressed={chip === c}
              disabled={disabled}
              onClick={() => setChip(c)}
            >
              {c}
            </button>
          );
        })}
        <span className={styles.liveInd}>
          <span className={styles.lamp} />
          {chip === "Logs" && !isCore ? "logs unavailable" : live ? "live" : "connecting…"}
        </span>
      </div>
      {!isCore && (
        <div className={styles.coreOnlyNote}>
          Logs are served only for controllers on the core cluster
        </div>
      )}
      <div className={styles.feed}>
        {streamError && (
          <div className={styles.mutedRow}>log stream reconnecting…</div>
        )}
        {entries.length === 0 && !streamError && (
          <div className={styles.mutedRow}>No stream entries yet.</div>
        )}
        {entries.map((e) =>
          e.kind === "activity" ? (
            <div key={e.key} className={`${styles.row} ${e.phase ? styles.phaseRow : ""}`}>
              <span className={styles.t}>{fmtStreamTime(e.timestamp)}</span>
              <span>{e.message}</span>
            </div>
          ) : (
            <div key={e.key} className={styles.row}>
              <span className={styles.t}>{fmtStreamTime(e.timestamp)}</span>
              <span className={styles.logline}>
                <span className={styles.lvlTxt}>{e.level}</span> {e.source}: {e.message}
              </span>
            </div>
          ),
        )}
      </div>
    </div>
  );
}

function fmtStreamTime(ts: string): string {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return "—";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

// ---- Apply history (moved from the Configuration tab) ----

function ApplyHistoryPanel({ ctrl }: { ctrl: ControllerData }) {
  const history = ctrl.applyHistory ?? [];
  const sorted = [...history].sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime());
  if (sorted.length === 0) return null;
  return (
    <Card title="Apply History">
      {sorted.map((ar, i) => (
        <div key={i} className={`${styles.historyEntry} ${ar.succeeded ? styles.historyOk : styles.historyBad}`}>
          <span className={styles.historyHash}>{shortHash(ar.hash)}</span>
          <span className={styles.historyTime}>{new Date(ar.timestamp).toLocaleString()}</span>
          <span>{ar.trigger || "reconciliation"}</span>
          <span>{ar.succeeded ? "✓" : "✗"}</span>
          {ar.sections && ar.sections.map((s, j) => (
            <span key={j} className={`${styles.historyChip} ${s.ok ? styles.historyChipOk : styles.historyChipBad}`}>
              {s.ok ? "✓" : "✗"} {s.name}
            </span>
          ))}
        </div>
      ))}
    </Card>
  );
}

function shortHash(h: string) {
  if (!h) return "";
  return h.length > 12 ? h.slice(0, 12) + "…" : h;
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

// --- Configuration Tab ---

function ConfigurationTab({
  ctrl, cluster, namespace, name,
  canUpdate,
}: {
  ctrl: NonNullable<ReturnType<typeof useController>["data"]>;
  cluster: string;
  namespace: string;
  name: string;
  canUpdate: boolean;
}) {
  const queryClient = useQueryClient();
  const [editingPolicy, setEditingPolicy] = useState(false);

  return (
    <div className={styles.configuration} data-testid="configuration-tab">
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
          The SpecEditorCard hydrates every tier (curated form, podOverrides,
          ingressSpec/miteSpec YAML, resource overlays) from ctrl.spec — the full
          ControllerSpec the BFF projects on the detail response. initialOverlay /
          initialPodOverrides remain as the pre-spec seed; the hydration effect
          overrides them from spec and keeps the immutable baseline for diffing.
        */}
        <SpecEditorCard cluster={cluster} ns={namespace} name={name} spec={ctrl.spec} initialOverlay={ctrl.resourceOverlay} initialPodOverrides={ctrl.podOverrides} canUpdate={canUpdate} />
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
  // "Upgrade pending release" chip: a manual-policy candidate promotion is
  // holding this controller. The upgradeBlocked guard keeps this mutually
  // exclusive with VersionRollBanner's blocking banner — a block always wins.
  const upgradePending = vs?.upgradeBlocked !== true && vs?.rollReason === "UpgradePending";
  const upgradePendingNote = upgradePending ? vs?.rollMessage : undefined;

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
        {rolling ? (
          <div className={styles.inProgressChip}>⟳ Upgrade in progress…</div>
        ) : upgradePending ? (
          <div className={styles.pendingReleaseChip}>
            Upgrade pending release
            {upgradePendingNote && <div className={styles.pendingMeta}>{upgradePendingNote}</div>}
          </div>
        ) : null}

        {inert ? (
          <div className={styles.versionMutedNote}>
            Version is pinned by a resourceOverlay statefulSet image override (
            <span className={styles.mono}>{inertImg}</span>). Edit the overlay to change the Jenkins image.
          </div>
        ) : upd ? (
          <VersionPicker
            versions={versions}
            value={target}
            onChange={setTarget}
            disabled={inert}
            upgradePendingNote={upgradePendingNote}
          />
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

  // Local edit state, hydrated from ctrl.spec?.hibernation. A plain
  // useState(initialProp) would read its argument only on the first render and
  // never track ctrl again; instead the hydration effect below re-syncs from
  // ctrl whenever its hibernation values (or the controller identity) change,
  // so another writer's background refetch and the conflict dialog's Reload
  // both reach this card. In-progress edits are never clobbered (dirtyVersion).
  const [enabled, setEnabled] = useState(false);
  const [gracePeriodMinutes, setGracePeriodMinutes] = useState(60);
  const [activityIgnoreRegex, setActivityIgnoreRegex] = useState("");
  const [saving, setSaving] = useState(false);
  const [conflicts, setConflicts] = useState<import("../api/client").ConflictInfo[]>([]);
  const [showConflict, setShowConflict] = useState(false);

  // Version-based dirty check, mirroring SpecEditorCard's per-tier
  // draftVersion (a tier is dirty iff its version > 0): any edit bumps it,
  // hydration and a successful save reset it. It is captured when a save
  // starts, so an edit made while the save was in flight keeps the card out of
  // the post-save rebase.
  const [dirtyVersion, setDirtyVersion] = useState(0);
  const dirtyVersionRef = useRef(dirtyVersion);
  dirtyVersionRef.current = dirtyVersion;
  const isDirty = dirtyVersion > 0;
  const markDirty = () => setDirtyVersion((v) => v + 1);

  // The baseline the card was last hydrated from — what a save is DIFFED
  // against. SpecEditorCard diffs each tier against its own hydration snapshot
  // so a save re-sends only what the user changed; a stale value that was never
  // touched must not be re-asserted (that would silently revert another
  // writer's concurrent change to the same field). This card applies the same
  // discipline to its three fields. hydrateFrom rewrites the baseline, so a
  // background refetch (pristine) and the post-save rebase both re-anchor it.
  const baselineRef = useRef({ enabled: false, gracePeriodMinutes: 60, activityIgnoreRegex: "" });

  // Hydrate the three fields from a server hibernation value, re-anchor the
  // diff baseline, and mark the card pristine.
  const hydrateFrom = (h: HibernationSpec | undefined) => {
    const baseline = {
      enabled: h?.enabled ?? false,
      gracePeriodMinutes: h?.gracePeriodMinutes ?? 60,
      activityIgnoreRegex: h?.activityIgnoreRegex ?? "",
    };
    baselineRef.current = baseline;
    setEnabled(baseline.enabled);
    setGracePeriodMinutes(baseline.gracePeriodMinutes);
    setActivityIgnoreRegex(baseline.activityIgnoreRegex);
    setDirtyVersion(0);
  };

  // Re-run past first render when the server's hibernation values change (a
  // background refetch — another writer, or the conflict dialog's Reload) or
  // when the controller identity changes. Never key on dirtyVersion itself: a
  // post-save rebase clears it before the refetch lands, and a dirty-keyed
  // effect would re-run against the still-stale ctrl and clobber the just-saved
  // values. The isDirty guard (read at effect-run time, not as a dep) keeps
  // in-progress edits on a refetch, while an identity change always hydrates.
  const identity = `${cluster}/${ns}/${name}`;
  const prevIdentity = useRef<string | null>(null);
  useEffect(() => {
    const idChanged = prevIdentity.current !== identity;
    prevIdentity.current = identity;
    if (idChanged || !isDirty) hydrateFrom(ctrl.spec?.hibernation);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ctrl.spec?.hibernation, identity]);

  const handleSave = async (force = false) => {
    // Diff against the hydration baseline so a save emits ONLY what the user
    // changed (SpecEditorCard's discipline). A field the user never touched is
    // never re-sent, so another writer's concurrent change to it is not
    // reverted. Clearing a previously-set activityIgnoreRegex still emits an
    // explicit null removal; an already-empty untouched regex is omitted — it
    // would otherwise turn every save into a removal request that can raise a
    // spurious "could not be removed" notice.
    const baseline = baselineRef.current;
    const hibernation: Record<string, unknown> = {};
    if (enabled !== baseline.enabled) hibernation.enabled = enabled;
    const gp = Number(gracePeriodMinutes);
    if (gp !== baseline.gracePeriodMinutes) hibernation.gracePeriodMinutes = gp;
    const trimmedRegex = activityIgnoreRegex.trim();
    // Compare the RAW value against the RAW baseline to decide whether the
    // field changed, then normalize only for sending. Trimming before the
    // comparison would make an untouched value with leading/trailing whitespace
    // look changed (trimmed !== untrimmed baseline), re-sending — and silently
    // rewriting — a regex the user never touched, which changes the spec and
    // rolls the pod.
    if (activityIgnoreRegex !== baseline.activityIgnoreRegex) {
      hibernation.activityIgnoreRegex = trimmedRegex === "" ? null : trimmedRegex;
    }
    // A save with no differences issues no request (8.4). State equals the
    // baseline, so the card is genuinely pristine: clear the dirty flag so the
    // hydration effect keeps re-syncing on background refetches. Without this,
    // an edit-and-revert (or a double toggle) would leave the card "dirty"
    // forever — the effect then skips hydration and the card silently goes
    // stale.
    if (Object.keys(hibernation).length === 0) {
      setDirtyVersion(0);
      return;
    }

    setSaving(true);
    try {
      const patch: Record<string, unknown> = { spec: { hibernation } };
      const savedVersion = dirtyVersionRef.current;
      const updated = await updateController(cluster, name, ns, patch, { force });
      // Rebase from the response so the card reflects what was saved and is no
      // longer dirty — unless the user kept editing while the save was in
      // flight (version moved since we captured it), in which case their newer
      // edits win and the card stays dirty.
      if (dirtyVersionRef.current === savedVersion) {
        hydrateFrom(updated?.spec?.hibernation);
      }
      queryClient.invalidateQueries({ queryKey: ["controller", cluster, ns, name] });
      setConflicts([]);
      setShowConflict(false);
      // Clearing activityIgnoreRegex sends an explicit null removal that
      // another manager may retain — surface that instead of an unqualified
      // success. The save itself succeeded either way.
      toast(unappliedRemovalNotice(updated) ?? "Hibernation settings updated");
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
            onChange={(e) => {
              setEnabled(e.target.checked);
              markDirty();
            }}
          />
        </div>
        <div className={styles.statLine}>
          <span className={styles.sk}>Grace period (min)</span>
          <input
            type="number"
            min={5}
            value={gracePeriodMinutes}
            onChange={(e) => {
              setGracePeriodMinutes(Number(e.target.value));
              markDirty();
            }}
            className={styles.policyInput}
          />
        </div>
        <div className={styles.statLine}>
          <span className={styles.sk}>Activity ignore regex</span>
        </div>
        <input
          type="text"
          value={activityIgnoreRegex}
          onChange={(e) => {
            setActivityIgnoreRegex(e.target.value);
            markDirty();
          }}
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
        // Reload means "discard my edits and show the server's state": hydrate
        // from the cached controller immediately (the card only renders with a
        // defined ctrl, so unlike SpecEditorCard no force flag is needed), then
        // invalidate so the hydration effect re-syncs on the refetch.
        hydrateFrom(ctrl.spec?.hibernation);
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
  if (vs.rollPending === true && vs.rollReason !== "VersionRollStarted" && vs.rollReason !== "UpgradePending") {
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

// AttentionBanner covers the attention kinds that have no dedicated banner.
// reconcileBlocked keeps ReconcileBlockedBanner; applyFailed is shown by the
// apply-result section below, so neither gets a hint here.
const ATTENTION_HINT: Partial<Record<ControllerAttentionKind, string>> = {
  bootFailed:
    "Jenkins is not reaching a healthy boot. Read the Jenkins container logs (Logs tab): a JCasC error such as a cloud without a name, or a plugin dependency that cannot load, fails boot. Fixing the bundle rolls the pod automatically.",
  pluginRollFailed:
    "The plugins-init step failed. The message above names the plugin; check that its pin exists in the update center and matches the JenkinsVersionProfile lock.",
  failed:
    "Reconciliation gave up. Fix the cause named above, then edit the spec or trigger a reconcile to retry.",
};

export function AttentionBanner({ attention }: { attention?: ControllerAttention }) {
  if (!attention || !(attention.kind in ATTENTION_HINT)) return null;
  return (
    <div className={styles.versionBannerBlocked} role="alert">
      <span className={styles.pendingIcon}>⚠</span>
      <div className={styles.pendingBody}>
        <div className={styles.pendingTitle}>{ATTENTION_LABEL[attention.kind]}</div>
        {attention.message && <div className={styles.pendingMeta}>{attention.message}</div>}
        <div>{ATTENTION_HINT[attention.kind]}</div>
        {attention.since && <div className={styles.pendingMeta}>since {new Date(attention.since).toLocaleString()}</div>}
      </div>
    </div>
  );
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
      // An empty interval/maxDeferSeconds/drainTimeoutSeconds is sent as an
      // explicit null removal that another manager may retain — surface that
      // instead of an unqualified success. The save itself succeeded either way.
      const updated = await updateController(cluster, name, namespace, patch);
      toast(unappliedRemovalNotice(updated) ?? "Reconciliation policy updated");
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
      {pending ? "⟳ Reconciling…" : "⟳ Reconcile"}
    </Button>
  );
}
