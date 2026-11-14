import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { useControllers } from "../../hooks/useControllers";
import { useClusters, coreOf } from "../../hooks/useClusters";
import { ClusterFanoutNotice } from "../../components/ClusterFanoutNotice";
import { age, activityTypeMeta, activitySubject } from "../../components/activityTimeline.util";
import { bffFetch, isRecordStoreUnreachable } from "../../hooks/useApi";
import { getUpdateCenterStatus } from "../../api/client";
import type { ActivityEvent, UpdateCenterStatus } from "../../types";
import { MetricCard } from "../../components/MetricCard";
import { Card } from "../../components/Card";
import { Pulse } from "../../components/Pulse";
import { StatusPill, ATTENTION_LABEL } from "../../components/StatusPill";
import { controllerRoute } from "../../routing";
import type { ControllerListItem } from "../../hooks/useControllers";
import styles from "./ControllersSection.module.css";

// HEALTH_LIST_LIMIT caps the brood health card. The card shows a phase bar
// for the whole brood and this many controller rows.
const HEALTH_LIST_LIMIT = 8;
const PHASE_BUCKETS = ["Connected", "Running", "Provisioning", "Hibernated", "Failed"] as const;

// phaseCounts counts controllers per phase bucket. Phases outside the buckets
// count as "Other". Zero buckets are dropped.
function phaseCounts(cs: ControllerListItem[]): { phase: string; count: number }[] {
  const byPhase = new Map<string, number>();
  for (const c of cs) {
    const bucket = (PHASE_BUCKETS as readonly string[]).includes(c.phase) ? c.phase : "Other";
    byPhase.set(bucket, (byPhase.get(bucket) ?? 0) + 1);
  }
  return [...PHASE_BUCKETS, "Other"]
    .map((phase) => ({ phase, count: byPhase.get(phase) ?? 0 }))
    .filter((x) => x.count > 0);
}

// lookRank orders controllers that need a look first. Attention comes first,
// then a running controller that lost its mite. Live controllers come before
// the ones that are asleep or stopped, which need no look.
function lookRank(c: ControllerListItem): number {
  if (c.attention) return 0;
  const parked = c.phase === "Hibernated" || c.phase === "Stopped";
  if (!c.miteConnected && !parked) return 1;
  if (parked) return 3;
  return 2;
}

// healthText names the health of a row. A parked controller reads as its
// state, not as an unreachable mite.
function healthText(c: ControllerListItem): string | undefined {
  if (c.phase === "Hibernated") return "asleep";
  if (c.phase === "Stopped") return "stopped";
  return c.jenkinsHealth;
}

function needsLookFirst(a: ControllerListItem, b: ControllerListItem): number {
  return lookRank(a) - lookRank(b) || a.name.localeCompare(b.name);
}

export function ControllersSection() {
  const { data, isLoading, error } = useControllers();

  const controllers = data ?? [];
  const total = controllers.length;
  const shortlist = useMemo(
    () => [...(data ?? [])].sort(needsLookFirst).slice(0, HEALTH_LIST_LIMIT),
    [data],
  );
  const phases = useMemo(() => phaseCounts(data ?? []), [data]);
  const rest = total - shortlist.length;
  const connected = controllers.filter((c) => c.miteConnected).length;
  const provisioning = controllers.filter((c) => c.phase === "Provisioning").length;
  const attention = controllers.filter((c) => c.attention);
  const attentionBreakdown = Object.entries(
    attention.reduce<Record<string, number>>((acc, c) => {
      const label = ATTENTION_LABEL[c.attention!.kind].toLowerCase();
      acc[label] = (acc[label] ?? 0) + 1;
      return acc;
    }, {}),
  )
    .map(([label, n]) => `${n} ${label}`)
    .join(" \u00b7 ");

  const head = (
    <div className={styles.sectionHead}>
      <h2 id="overview-controllers" className={styles.sectionTitle}>Controllers</h2>
      <Link to="/controllers/create" className={styles.btnPrimary}>
        ＋ New controller
      </Link>
    </div>
  );

  if (isLoading) {
    return (
      <section className={styles.section} aria-labelledby="overview-controllers">
        {head}
        <div className={`${styles.grid} ${styles.metrics}`}>
          {[1, 2, 3, 4].map((i) => (
            <div key={i} className={styles.skeletonMetric} data-testid="metric-skeleton" />
          ))}
        </div>
      </section>
    );
  }

  if (error) {
    return (
      <section className={styles.section} aria-labelledby="overview-controllers">
        {head}
        <div className={styles.errorBanner}>
          Failed to load brood data: {error.message}
        </div>
      </section>
    );
  }

  return (
    <section className={styles.section} aria-labelledby="overview-controllers">
      {head}
      <ClusterFanoutNotice />

      <div className={`${styles.grid} ${styles.metrics}`}>
        <MetricCard
          label="Total controllers"
          value={total}
          icon={<span>⬡</span>}
          accent="accent"
        />
        <MetricCard
          label="Mites connected"
          value={`${connected} / ${total}`}
          sub={
            <>
              <Pulse active size={8} /> streaming live
            </>
          }
          icon={<span>⚯</span>}
          accent="ok"
        />
        <MetricCard
          label="Provisioning"
          value={provisioning}
          sub={provisioning > 0 ? `${provisioning} in progress` : "none"}
          icon={<span>◴</span>}
          accent="warn"
        />
        <MetricCard
          label="Needs attention"
          value={attention.length}
          sub={attention.length > 0 ? attentionBreakdown : "all clear"}
          icon={<span>⚠</span>}
          accent={attention.length > 0 ? "bad" : "ok"}
        />
      </div>

      <UpdateCenterUnservableChip />

      <div className={styles.twoCol}>
        <Card title="⬡ Brood health" headerRight={<span className={styles.muted}>{controllers.length} controller{controllers.length === 1 ? "" : "s"}</span>}>
          <div className={styles.healthGrid}>
            {total === 0 ? (
              <div className={styles.empty}>No controllers deployed yet.</div>
            ) : (
              <>
                <PhaseBar counts={phases} />
                {shortlist.map((c) => (
                  <HealthBar key={`${c.cluster}/${c.namespace}/${c.name}`} controller={c} />
                ))}
                {rest > 0 && (
                  <div className={styles.healthMore}>
                    and {rest} more ·{" "}
                    <Link to="/controllers" className={styles.healthLink}>View all controllers</Link>
                  </div>
                )}
              </>
            )}
          </div>
        </Card>

        <ActivityFeed />
      </div>
    </section>
  );
}

// PhaseBar draws the phase distribution of the whole brood as one stacked bar,
// with a legend of counts per phase.
function PhaseBar({ counts }: { counts: { phase: string; count: number }[] }) {
  const label = "Phase distribution: " + counts.map((x) => `${x.count} ${x.phase}`).join(", ");
  return (
    <div>
      <div className={styles.phaseBar} role="img" aria-label={label}>
        {counts.map((x) => (
          <span key={x.phase} className={styles.phaseSeg} data-phase={x.phase} style={{ flexGrow: x.count }} />
        ))}
      </div>
      <ul className={styles.phaseLegend}>
        {counts.map((x) => (
          <li key={x.phase}>
            <span className={styles.phaseDot} data-phase={x.phase} /> {x.phase} <b>{x.count}</b>
          </li>
        ))}
      </ul>
    </div>
  );
}

function HealthBar({ controller }: { controller: ControllerListItem }) {
  return (
    <div data-testid="health-row">
      <div className={styles.healthLabel}>
        <span style={{ display: "flex", gap: 8, alignItems: "center" }}>
          <Pulse active={controller.miteConnected} size={8} />
          <Link to={controllerRoute(controller.cluster, controller.namespace, controller.name)} className={styles.healthLink} aria-label={`View details for ${controller.namespace}/${controller.name}`}>{controller.name}</Link>
        </span>
        <StatusPill phase={controller.phase} attention={controller.attention} size="sm" />
      </div>
      <div className={styles.healthMeta}>
        <span>{controller.lastSeen ? `seen ${age(controller.lastSeen)}` : "never seen"}</span>
        {healthText(controller) && <span>{healthText(controller)}</span>}
        {controller.jenkinsVersion && <span>Jenkins {controller.jenkinsVersion}</span>}
      </div>
    </div>
  );
}

function ActivityFeed() {
  const { data: clusters } = useClusters();
  const core = coreOf(clusters);
  // The server leaves out routine local sign-ins. The activity page lists them.
  const { data: events = [], error, refetch } = useQuery({
    queryKey: ["activity"],
    queryFn: () => bffFetch<{items: ActivityEvent[]}>("/activity?limit=5&excludeRoutineSignIns=true").then(r => r.items),
    refetchInterval: 10_000,
  });
  const unreachable = isRecordStoreUnreachable(error);

  return (
    <Card
      title="◷ Recent activity"
      headerRight={
        <Link to="/activity" className={styles.activityLink}>
          View all
        </Link>
      }
    >
      <div className={styles.activityList}>
        {unreachable && (
          <div className={styles.activityUnavailable} role="alert">
            <span>The build record store did not answer, so activity is not available.</span>
            <button type="button" className={styles.activityRetry} onClick={() => void refetch()}>
              Retry
            </button>
          </div>
        )}
        {!unreachable && events.map((e, i) => {
          const { icon, style } = activityTypeMeta(e);
          const { subject, message } = activitySubject(e);
          return (
            <div key={i} className={styles.activityItem}>
              <div className={styles.activityIcon} data-style={style}>
                {icon}
              </div>
              <div className={styles.activityContent}>
                <div className={styles.activityTitle}>
                  {subject && (
                    <>
                      <b>{subject}</b>{" "}
                    </>
                  )}
                  {message}
                </div>
                <div className={styles.activityMeta}>
                  {e.cluster && e.cluster !== core?.name ? `${e.cluster}/` : ""}
                  {e.namespace && `ns/${e.namespace} · `}
                  <span title={new Date(e.timestamp).toLocaleString()}>{age(e.timestamp)}</span>
                </div>
              </div>
            </div>
          );
        })}
        {!unreachable && events.length === 0 && (
          <div className={styles.activityEmpty}>
            No activity yet. Events will appear when mites connect or phases change.
          </div>
        )}
      </div>
    </Card>
  );
}

function UpdateCenterUnservableChip() {
  const [data, setData] = useState<UpdateCenterStatus | null>(null);
  useEffect(() => {
    getUpdateCenterStatus()
      .then(setData)
      .catch(() => {
        // Swallow — failed fetch must not surface a dashboard error.
      });
  }, []);
  if (!data?.enabled || !data.unservableCount) return null;
  return (
    <Link
      to="/administration/update-center"
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: "6px 14px",
        marginBottom: 16,
        borderRadius: 8,
        background: "var(--warn-soft, rgba(255, 183, 0, 0.12))",
        color: "var(--warn-text, #b8860b)",
        fontSize: ".85rem",
        fontWeight: 600,
        textDecoration: "none",
      }}
    >
      ⚠ {data.unservableCount} plugin{data.unservableCount > 1 ? "s" : ""} the Update Center cannot serve
    </Link>
  );
}
