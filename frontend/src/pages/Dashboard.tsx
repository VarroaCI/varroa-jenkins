import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { useControllers } from "../hooks/useControllers";
import { useClusters, coreOf } from "../hooks/useClusters";
import { age } from "../components/activityTimeline.util";
import { bffFetch } from "../hooks/useApi";
import { getUpdateCenterStatus } from "../api/client";
import type { UpdateCenterStatus } from "../types";
import { MetricCard } from "../components/MetricCard";
import { Card } from "../components/Card";
import { Pulse } from "../components/Pulse";
import { StatusPill, ATTENTION_LABEL } from "../components/StatusPill";
import { controllerRoute } from "../routing";
import type { ControllerListItem } from "../hooks/useControllers";
import type { ClusterEntry } from "../types";
import styles from "./Dashboard.module.css";

export default function Dashboard() {
  const { data, isLoading, error } = useControllers();
  const { data: clustersData, isLoading: clustersLoading, error: clustersError } = useClusters();

  const controllers = data ?? [];
  const total = controllers.length;
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
  // Namespaces are only unique within a cluster — the same name exists in more
  // than one cluster, so count (cluster, namespace) identities.
  const namespaceCount = new Set(controllers.map((c) => `${c.cluster}/${c.namespace}`)).size;

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.pageHead}>
          <div>
            <div className={styles.pageTitle}>Brood overview</div>
            <div className={styles.pageDesc}>Loading...</div>
          </div>
        </div>
        <div className={`${styles.grid} ${styles.metrics}`}>
          {[1, 2, 3, 4].map((i) => (
            <div key={i} className={styles.skeletonMetric} />
          ))}
        </div>
      </div>
    );
  }

  if (error) {
    return (
      <div className={styles.page}>
        <div className={styles.errorBanner}>
          Failed to load brood data: {error.message}
        </div>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Brood overview</div>
          <div className={styles.pageDesc}>
            {total} controller{total === 1 ? "" : "s"} across {namespaceCount} namespace
            {namespaceCount === 1 ? "" : "s"}
          </div>
        </div>
        <div style={{ display: "flex", gap: 10 }}>
          <Link to="/controllers/create" className={styles.btnPrimary}>
            ＋ New controller
          </Link>
        </div>
      </div>

      <ClusterStrip clusters={clustersData} isLoading={clustersLoading} error={!!clustersError} />

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

      <UpdateCenterGapsChip />

      <div className={styles.twoCol}>
        <Card title="⬡ Brood health" headerRight={<span className={styles.muted}>{controllers.length} controller{controllers.length === 1 ? "" : "s"}</span>}>
          <div className={styles.healthGrid}>
            {controllers.map((c) => (
              <HealthBar key={`${c.namespace}/${c.name}`} controller={c} />
            ))}
            {controllers.length === 0 && (
              <div className={styles.empty}>No controllers deployed yet.</div>
            )}
          </div>
        </Card>

        <ActivityFeed />
      </div>
    </div>
  );
}

function ClusterStrip({ clusters, isLoading, error }: { clusters: ClusterEntry[] | undefined; isLoading: boolean; error: boolean }) {
  if (isLoading || error || (clusters?.length ?? 0) < 2) return null;
  const cls = [...(clusters ?? [])].sort((a, b) => (a.core ? -1 : b.core ? 1 : a.name.localeCompare(b.name)));
  return (
    <div className={styles.clusterStrip}>
      {cls.map((c) => (
        <Link key={c.name} to={`/controllers?cluster=${encodeURIComponent(c.name)}`} className={styles.clusterCard}>
          <div className={styles.clusterCardHead}>
            <span className={styles.clusterDot} data-health={c.healthy ? "ok" : "bad"} />
            <span className={styles.clusterName}>{c.name}</span>
            {c.core && <span className={styles.coreTag}>core</span>}
          </div>
          <div className={styles.clusterCardStat}>{c.connectedCount}/{c.controllerCount} controllers</div>
          <div className={styles.clusterCardAge}>{age(c.lastHeartbeat, { variant: "heartbeat" })}</div>
        </Link>
      ))}
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
        {controller.jenkinsHealth && <span>{controller.jenkinsHealth}</span>}
        {controller.jenkinsVersion && <span>Jenkins {controller.jenkinsVersion}</span>}
      </div>
    </div>
  );
}

interface ActivityEvent {
  timestamp: string;
  type: string;
  controller: string;
  namespace: string;
  cluster?: string;
  message: string;
}

const ACTIVITY_TYPES: Record<string, { icon: string; style: string }> = {
  connected: { icon: "⇡", style: "softInfo" },
  disconnected: { icon: "⇣", style: "softBad" },
  phase: { icon: "◴", style: "softWarn" },
  reconciled: { icon: "✓", style: "softOk" },
};

function ActivityFeed() {
  const { data: clusters } = useClusters();
  const core = coreOf(clusters);
  const { data: events = [] } = useQuery({
    queryKey: ["activity"],
    queryFn: () => bffFetch<{items: ActivityEvent[]}>("/activity").then(r => r.items),
    refetchInterval: 10_000,
  });

  const icons = (t: string) => ACTIVITY_TYPES[t] ?? { icon: "·", style: "" };

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
        {events.slice(0, 5).map((e, i) => {
          const { icon, style } = icons(e.type);
          return (
            <div key={i} className={styles.activityItem}>
              <div className={styles.activityIcon} data-style={style}>
                {icon}
              </div>
              <div className={styles.activityContent}>
                <div className={styles.activityTitle}>
                  <b>{e.controller || "system"}</b> {e.message}
                </div>
                <div className={styles.activityMeta}>
                  {e.cluster && e.cluster !== core?.name ? `${e.cluster}/` : ""}{e.namespace && `ns/${e.namespace} · `}{new Date(e.timestamp).toLocaleTimeString()}
                </div>
              </div>
            </div>
          );
        })}
        {events.length === 0 && (
          <div className={styles.activityEmpty}>
            No activity yet. Events will appear when mites connect or phases change.
          </div>
        )}
      </div>
    </Card>
  );
}

function UpdateCenterGapsChip() {
  const [data, setData] = useState<UpdateCenterStatus | null>(null);
  useEffect(() => {
    getUpdateCenterStatus()
      .then(setData)
      .catch(() => {
        // Swallow — failed fetch must not surface a dashboard error.
      });
  }, []);
  if (!data?.enabled || !data.gaps || data.gaps.length === 0) return null;
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
      ⚠ {data.gaps.length} plugin{data.gaps.length > 1 ? "s" : ""} missing from Update Center
    </Link>
  );
}
