import { Fragment } from "react";
import type { ControllerDetail } from "../hooks/useControllers";
import type { ControllerDiff } from "../types";
import { age } from "./activityTimeline.util";
import styles from "./DeliveryPipeline.module.css";

export type LampState = "ok" | "live" | "warn" | "bad" | "wait";
export type RailState = "flow" | "busy" | "off";
export type CheckState = "ok" | "bad" | "run" | "wait";
export type StepState = "done" | "now" | "todo";

export interface SubSegment {
  t: string;
  mono?: boolean;
}

export interface DeliveryCheck {
  label: string;
  state: CheckState;
}

export interface DeliveryStep {
  label: string;
  state: StepState;
}

export interface DeliveryNode {
  id: "bundle" | "apply" | "jenkins";
  name: string;
  what: string;
  lamp: LampState;
  line: string;
  sub?: SubSegment[];
  error?: string;
  checks?: DeliveryCheck[];
  steps?: DeliveryStep[];
}

export interface DeliveryRails {
  bundleToApply: RailState;
  applyToJenkins: RailState;
}

/** What the pipeline resolves to for a given controller snapshot. */
export type DeliveryModel =
  | { kind: "quiet"; powerLabel: "Hibernated" | "Stopped"; hash: string; jobs: number | undefined }
  | { kind: "pipeline"; nodes: DeliveryNode[]; rails: DeliveryRails };

const HIBERNATED = "Hibernated";
const STOPPED = "Stopped";

/** True when the controller is scaled to zero (hibernated or stopped). */
export function isAsleep(ctrl: ControllerDetail): boolean {
  return (
    ctrl.hibernated === true ||
    ctrl.phase === HIBERNATED ||
    ctrl.powerState === STOPPED ||
    ctrl.phase === STOPPED
  );
}

function short8(h?: string): string {
  if (!h) return "";
  return h.length > 8 ? h.slice(0, 8) : h;
}

function bundleName(ctrl: ControllerDetail): string {
  return ctrl.effectiveBundle?.name ?? ctrl.composedBundleRef?.name ?? "varroa-starter";
}

function rel(ts?: string, now?: number): string {
  if (!ts) return "—";
  return age(ts, { variant: "heartbeat", ...(now !== undefined ? { now } : {}) });
}

/**
 * Derive the three delivery nodes (Bundle → Apply → Jenkins) plus the rails
 * between them from the controller snapshot. Exported for unit testing.
 */
export function buildDeliveryNodes(ctrl: ControllerDetail, now?: number): DeliveryModel {
  if (isAsleep(ctrl)) {
    const powerLabel: "Hibernated" | "Stopped" =
      ctrl.powerState === STOPPED || ctrl.phase === STOPPED ? "Stopped" : "Hibernated";
    const hash = short8(ctrl.lastApplyResult?.hash || ctrl.appliedBundleHash || ctrl.desiredStateHash || "");
    // undefined (not 0) when observability/summary is entirely absent — "no
    // data" must stay distinct from a real zero-job count.
    const jobs = ctrl.observability?.summary?.totalJobs;
    return { kind: "quiet", powerLabel, hash, jobs };
  }

  /* ── Bundle node ── */
  const bundleHash = short8(ctrl.appliedBundleHash);
  const bundle: DeliveryNode = {
    id: "bundle",
    name: "Bundle",
    what: "what to run",
    lamp: ctrl.appliedBundleHash ? "ok" : "wait",
    line: bundleName(ctrl),
    // Only show the separator + hash segment when a bundle hash is actually
    // present, otherwise the sub-line would render a dangling "composed · ".
    // Mirrors the applySub guard for the Apply node below.
    sub: bundleHash ? [{ t: "composed · " }, { t: bundleHash, mono: true }] : [{ t: "composed" }],
  };

  /* ── Apply node ── */
  const mode = ctrl.reconciliationPolicy?.mode || "automatic";
  const hashMismatch =
    !!ctrl.desiredStateHash && !!ctrl.lastApplyResult?.hash && ctrl.lastApplyResult.hash !== ctrl.desiredStateHash;
  // A hash mismatch in manual mode means the change is parked awaiting operator
  // approval, not actively being applied — keep the two states distinct.
  const applying = hashMismatch && mode !== "manual";
  const awaitingManualApproval = hashMismatch && mode === "manual";
  const applyHash = short8(ctrl.lastApplyResult?.hash || ctrl.desiredStateHash || "");
  const sections = ctrl.lastApplyResult?.sections ?? [];
  const failed = sections.filter((s) => !s.ok);

  let lamp: LampState;
  let line: string;
  let errorText: string | undefined;

  if (ctrl.lastApplyResult?.succeeded === false) {
    // A real apply failure is the concrete outcome and takes precedence over
    // both the parked (manual) and in-flight (automatic) interpretations.
    lamp = "bad";
    const names = failed.map((s) => s.name);
    line = names.length ? `${names.join(", ")} rejected` : "apply rejected";
    const errs = failed.map((s) => s.error).filter((e): e is string => Boolean(e));
    errorText = failed.length ? (errs.length ? errs.join("; ") : "rejected, no message returned") : undefined;
  } else if (awaitingManualApproval) {
    lamp = "warn";
    line = "awaiting manual approval";
  } else if (applying) {
    lamp = "warn";
    line = "applying new configuration…";
  } else if (ctrl.lastApplyResult) {
    lamp = "ok";
    line = `applied ${rel(ctrl.lastApplyResult.timestamp, now)}`;
  } else {
    lamp = "wait";
    line = "not applied yet";
  }

  // While applying the checks spin as "run", but only while the outcome is
  // genuinely unknown. If the last apply already finished and failed, the
  // failure branch above takes precedence for the lamp/line; the per-section
  // checks must reflect the real ok/bad outcome rather than keep spinning.
  const activelyApplying = applying && ctrl.lastApplyResult?.succeeded !== false;
  const checks: DeliveryCheck[] = sections.map((s) => ({
    label: s.name,
    state: activelyApplying ? "run" : s.ok ? "ok" : "bad",
  }));

  const modeText =
    mode === "manual" ? "manual" :
    mode === "idle" ? "idle" :
    `automatic · every ${ctrl.reconciliationPolicy?.interval || "30s"}`;
  const applySub: SubSegment[] = [
    { t: applyHash ? `${modeText} · ` : modeText },
    ...(applyHash ? [{ t: applyHash, mono: true }] : []),
  ];

  const apply: DeliveryNode = {
    id: "apply",
    name: "Apply",
    what: "delivered to Jenkins",
    lamp,
    line,
    error: errorText,
    sub: applySub,
    checks: checks.length > 0 ? checks : undefined,
  };

  /* ── Jenkins node ── */
  let jenkins: DeliveryNode;
  // Pending is a transient wake phase (Hibernated → Pending → Provisioning →
  // Running → Connected): the replica exists but the operator has not started
  // provisioning yet. It must render as in-flight/waking, NOT as the
  // unexpected-offline "mite disconnected" branch.
  if (ctrl.phase === "Provisioning" || ctrl.phase === "Running" || ctrl.phase === "Pending") {
    const pending = ctrl.phase === "Pending";
    jenkins = {
      id: "jenkins",
      name: "Jenkins",
      what: "waking",
      lamp: "warn",
      line: pending ? "waking…" : "starting pod…",
      // Pending: nothing has started — Provisioning is the upcoming step and
      // all three steps are still not-done.
      steps: pending
        ? [
            { label: "Provisioning", state: "now" },
            { label: "Running", state: "todo" },
            { label: "Connected", state: "todo" },
          ]
        : [
            { label: "Provisioning", state: ctrl.phase === "Provisioning" ? "now" : "done" },
            { label: "Running", state: ctrl.phase === "Running" ? "now" : "todo" },
            { label: "Connected", state: "todo" },
          ],
    };
  } else if (ctrl.miteConnected) {
    jenkins = {
      id: "jenkins",
      name: "Jenkins",
      what: "live",
      lamp: "live",
      line: `${ctrl.jenkinsVersion || "unknown"} · ${ctrl.jenkinsHealth || "unknown"}`,
      sub: [{ t: `mite ${ctrl.miteVersion || "unknown"} · heartbeat ` }, { t: rel(ctrl.lastSeen, now) }],
    };
  } else {
    jenkins = {
      id: "jenkins",
      name: "Jenkins",
      what: "offline",
      lamp: "bad",
      line: "mite disconnected",
      sub: [{ t: "last heartbeat " }, { t: rel(ctrl.lastSeen, now) }],
    };
  }

  // Rail semantics (task #483 + mockup): a rail is busy when either end is
  // warn (in-flight apply or waking jenkins moves the whole line), off when
  // either end is bad/asleep/wait (a failed apply breaks the path to Jenkins;
  // a never-applied controller has nothing delivered yet), otherwise flow.
  const railBetween = (upstream: DeliveryNode, downstream: DeliveryNode): RailState => {
    // "off" wins over "busy" for EITHER end: a bad/wait lamp on the upstream
    // (failed/never-applied apply) or the downstream (dead/asleep jenkins)
    // breaks the path, even if the other end is "warn" (in-flight/waking).
    if (
      upstream.lamp === "bad" || upstream.lamp === "wait" ||
      downstream.lamp === "bad" || downstream.lamp === "wait"
    ) return "off";
    if (upstream.lamp === "warn" || downstream.lamp === "warn") return "busy";
    return "flow";
  };

  return {
    kind: "pipeline",
    nodes: [bundle, apply, jenkins],
    rails: {
      bundleToApply: railBetween(bundle, apply),
      applyToJenkins: railBetween(apply, jenkins),
    },
  };
}

const CHK_PREFIX: Record<CheckState, string> = { ok: "✓", bad: "✗", run: "⟳", wait: "…" };
const CHK_CLASS: Record<CheckState, string> = { ok: "", bad: "bad", run: "run", wait: "wait" };
const LAMP_CLASS: Record<LampState, string> = { ok: "ok", live: "live", warn: "warn", bad: "bad", wait: "wait" };

interface Props {
  ctrl: ControllerDetail;
  diff?: ControllerDiff | null;
  diffLoading?: boolean;
  diffError?: string | null;
  onFetchDiff?: () => void;
}

/** Bundle → Apply → Jenkins, or the quiet card when the controller sleeps. */
export function DeliveryPipeline({ ctrl, diff, diffLoading, diffError, onFetchDiff }: Props) {
  const model = buildDeliveryNodes(ctrl);

  if (model.kind === "quiet") {
    return (
      <div className={styles.quietWrap} data-testid="quiet-card">
        <div className={styles.moon}>☾</div>
        <h3>{model.powerLabel === "Stopped" ? "Powered off" : "Sleeping — scaled to zero"}</h3>
        <p>
          {model.powerLabel === "Stopped"
            ? "Stopped — not serving. Power on to resume."
            : "Wakes on the first inbound request, or wake it now. Nothing is lost while it sleeps."}
        </p>
        <div className={styles.kept}>
          config preserved at <span className={styles.mono}>{model.hash || "—"}</span>
          <span>·</span>
          <span>{model.jobs ?? "—"} jobs</span>
        </div>
      </div>
    );
  }

  const [bundle, apply, jenkins] = model.nodes;

  return (
    <div className={styles.pipe} data-testid="delivery-pipeline">
      <NodeView node={bundle} />
      <div className={`${styles.rail} ${styles[model.rails.bundleToApply]}`} data-testid="rail-bundle-apply" data-rail={model.rails.bundleToApply} />
      <NodeView
        node={apply}
        diff={diff}
        diffLoading={diffLoading}
        diffError={diffError}
        onFetchDiff={onFetchDiff}
      />
      <div className={`${styles.rail} ${styles[model.rails.applyToJenkins]}`} data-testid="rail-apply-jenkins" data-rail={model.rails.applyToJenkins} />
      <NodeView node={jenkins} />
    </div>
  );
}

function NodeView({
  node,
  diff,
  diffLoading,
  diffError,
  onFetchDiff,
}: {
  node: DeliveryNode;
  diff?: ControllerDiff | null;
  diffLoading?: boolean;
  diffError?: string | null;
  onFetchDiff?: () => void;
}) {
  return (
    <div className={`${styles.node} ${node.error ? styles.err : ""}`} data-node={node.id}>
      <div className={styles.top}>
        <span className={`${styles.lamp} ${styles[LAMP_CLASS[node.lamp]]}`} data-lamp={node.lamp} />
        <span className={styles.name}>{node.name}</span>
        <span className={styles.what}>{node.what}</span>
      </div>
      <div className={styles.line}>{node.line}</div>
      {node.sub && (
        <div className={styles.sub}>
          {node.sub.map((seg, i) =>
            seg.mono ? (
              <span key={i} className={styles.mono}>{seg.t}</span>
            ) : (
              <span key={i}>{seg.t}</span>
            ),
          )}
        </div>
      )}
      {node.steps && (
        <div className={styles.steps}>
          {node.steps.map((s, i) => (
            <Fragment key={s.label}>
              {i > 0 && <span className={styles.stepGap} />}
              <span className={`${styles.step} ${s.state === "todo" ? "" : styles[s.state]}`}>
                <span className={styles.pt} />{s.label}
              </span>
            </Fragment>
          ))}
        </div>
      )}
      {node.checks && node.checks.length > 0 && (
        <div className={styles.checks}>
          {node.checks.map((c) => (
            <span key={c.label} className={`${styles.chk} ${CHK_CLASS[c.state] ? styles[CHK_CLASS[c.state]] : ""}`} data-check={c.state}>
              {CHK_PREFIX[c.state]} {c.label}
            </span>
          ))}
        </div>
      )}
      {node.error && <div className={styles.errText}>{node.error}</div>}
      {node.id === "apply" && node.error && onFetchDiff && (
        <div className={styles.diffArea}>
          <button className={styles.diffLink} onClick={onFetchDiff} disabled={diffLoading}>
            {diffLoading ? "Loading diff…" : "view diff"}
          </button>
          {diffError && <span className={styles.diffError}>{diffError}</span>}
          {diff && diff.appliedUnavailable && (
            <div className={styles.diffNotice}>last-applied unavailable (mite offline)</div>
          )}
          {diff && !diff.appliedUnavailable && diff.incoming && (
            <pre className={styles.diffContent}>{truncate(diff.incoming.jcasc, 500)}</pre>
          )}
        </div>
      )}
    </div>
  );
}

function truncate(s: string, n: number) {
  if (!s) return "";
  return s.length > n ? s.slice(0, n) + "…" : s;
}
