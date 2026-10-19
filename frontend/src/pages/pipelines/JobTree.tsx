import type { CSSProperties, KeyboardEvent } from "react";
import { Link } from "react-router-dom";
import { ChevronRight, CloudRain, CloudSun, Sun, type LucideIcon } from "lucide-react";
import { age } from "../../components/activityTimeline.util";
import type { PipelineHealth, PipelineSummary } from "../../types";
import { breakablePath } from "./breakablePath";
import { isFoldable, type JobTreeNode } from "./jobTree";
import styles from "./JobTree.module.css";

const enc = (s: string) => encodeURIComponent(s);

function jobHref(team: string, jobPath: string): string {
  return `/pipelines/${enc(team)}/${enc(jobPath)}`;
}

function runHref(team: string, jobPath: string, runId: string): string {
  return `${jobHref(team, jobPath)}/${enc(runId)}`;
}

/** formatDuration renders a millisecond span as the two coarsest non-zero
 * units, dropping finer ones. */
export function formatDuration(ms: number | undefined): string {
  if (ms === undefined || ms < 0) return "—";
  const totalSec = Math.round(ms / 1000);
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

const HEALTH_GLYPH: Record<"clear" | "cloudy" | "rain", { Icon: LucideIcon; label: string; cls: string }> = {
  clear: { Icon: Sun, label: "Clear: none of the last five runs failed", cls: styles.healthClear },
  cloudy: { Icon: CloudSun, label: "Cloudy: one or two of the last five runs failed", cls: styles.healthCloudy },
  rain: { Icon: CloudRain, label: "Rain: three or more of the last five runs failed", cls: styles.healthRain },
};

/**
 * HealthGlyph renders the weather glyph naming its own state, so a screen
 * reader announces the state rather than the image. A job that has never
 * run carries health "unknown" and renders no glyph.
 */
function HealthGlyph({ health }: { health: PipelineHealth }) {
  if (health === "unknown") return null;
  const { Icon, label, cls } = HEALTH_GLYPH[health];
  return (
    <span className={`${styles.healthCell} ${cls}`} role="img" aria-label={label} title={label}>
      <Icon aria-hidden="true" size={18} />
    </span>
  );
}

/** Sparkline draws the last-8-durations trend as one inline SVG polyline.
 * No charting package is added for this. */
function Sparkline({ values }: { values: number[] }) {
  if (values.length === 0) return null;
  const w = 64;
  const h = 20;
  const max = Math.max(...values, 1);
  const min = Math.min(...values, 0);
  const range = max - min || 1;
  const step = values.length > 1 ? w / (values.length - 1) : 0;
  const points = values
    .map((v, i) => `${(i * step).toFixed(1)},${(h - ((v - min) / range) * h).toFixed(1)}`)
    .join(" ");
  return (
    <svg
      className={styles.sparkline}
      width={w}
      height={h}
      viewBox={`0 0 ${w} ${h}`}
      role="img"
      aria-label="Duration trend, last 8 builds"
    >
      <polyline points={points} fill="none" stroke="currentColor" strokeWidth={1.5} />
    </svg>
  );
}

/** rowStyle carries the node's depth to the stylesheet, which turns it into
 * padding. The custom property is declared in JobTree.module.css. */
function rowStyle(depth: number): CSSProperties {
  return { "--depth": String(depth) } as CSSProperties;
}

/** countLabel names a node's own count: jobs below a folder, branches under
 * a multibranch item, repositories under an organization folder. */
function countLabel(node: JobTreeNode): string {
  if (node.kind === "multibranch") {
    return node.branchCount === 1 ? "1 branch" : `${node.branchCount} branches`;
  }
  if (node.kind === "organizationFolder") {
    return node.repositoryCount === 1 ? "1 repository" : `${node.repositoryCount} repositories`;
  }
  return node.jobCount === 1 ? "1 job" : `${node.jobCount} jobs`;
}

/**
 * FolderRow renders a folder node, a multibranch node, or an organization
 * folder node. It fills the Pipeline column only: Varroa reports no aggregate
 * health for a folder, and a multibranch item or an organization folder does
 * not build.
 */
function FolderRow({
  team,
  folder,
  node,
  open,
  onToggle,
}: {
  team: string;
  folder: string;
  node: JobTreeNode;
  open: boolean;
  onToggle: (path: string, next: boolean) => void;
}) {
  // The arrow keys are bound to the button, never to the document, so they
  // cannot collide with the build page's single-key navigation.
  const onKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key === "ArrowRight" && !open) {
      event.preventDefault();
      onToggle(node.path, true);
    } else if (event.key === "ArrowLeft" && open) {
      event.preventDefault();
      onToggle(node.path, false);
    }
  };

  return (
    <tr className={styles.row} style={rowStyle(node.depth)}>
      <td className={`${styles.nameCell} ${styles.pipelineCell}`}>
        <span className={styles.nodeLine}>
          <button
            type="button"
            className={styles.disclosure}
            aria-expanded={open}
            aria-label={`${node.path}, ${countLabel(node)}`}
            onClick={() => onToggle(node.path, !open)}
            onKeyDown={onKeyDown}
          >
            <ChevronRight
              aria-hidden="true"
              size={16}
              className={`${styles.chevron} ${open ? styles.chevronOpen : ""}`}
            />
          </button>
          {node.kind === "multibranch" || node.kind === "organizationFolder" ? (
            <Link className={styles.nodeName} to={jobHref(team, folder ? `${folder}/${node.path}` : node.path)}>
              {node.name}
            </Link>
          ) : (
            <span className={styles.nodeName}>{node.name}</span>
          )}
          <span className={styles.counts}>{countLabel(node)}</span>
          {(node.kind === "multibranch" || node.kind === "organizationFolder") &&
            node.summary?.pending && <PendingBadge />}
          {node.runningCount > 0 && (
            <span className={styles.running}>{node.runningCount} running</span>
          )}
        </span>
      </td>
      <td />
      <td />
      <td />
      <td />
      <td />
      <td />
    </tr>
  );
}

/** PendingBadge marks a declared job the cell has not created yet. */
function PendingBadge() {
  return (
    <span className={styles.pendingBadge} title="The bundle declares this job, but no cell has created it yet.">
      Pending
    </span>
  );
}

/** JobRow renders one job node. It shows its own path segment, and carries
 * the whole job path in text only a screen reader reads. */
function JobRow({ team, node, item }: { team: string; node: JobTreeNode; item: PipelineSummary }) {
  return (
    <tr className={styles.row} style={rowStyle(node.depth)}>
      <td className={`${styles.nameCell} ${styles.pipelineCell}`}>
        <Link to={jobHref(team, item.jobPath)}>{node.name}</Link>
        <span className={styles.hiddenPath}>{item.jobPath}</span>
        {item.pending && <PendingBadge />}
      </td>
      <td>
        <HealthGlyph health={item.health} />
      </td>
      <td>
        {item.lastBuild ? (
          <Link to={runHref(team, item.jobPath, item.lastBuild.runId)}>
            #{item.lastBuild.buildNumber ?? "—"}
          </Link>
        ) : (
          <span className={styles.dim}>—</span>
        )}
      </td>
      <td className={styles.branchCell}>
        {item.lastBuild?.branch ? breakablePath(item.lastBuild.branch) : <span className={styles.dim}>—</span>}
      </td>
      <td>
        <div className={styles.durationCell}>
          {item.durationTrendMs && item.durationTrendMs.length > 0 && <Sparkline values={item.durationTrendMs} />}
          <span className={styles.lastDuration}>{formatDuration(item.lastBuild?.durationMs)}</span>
        </div>
      </td>
      <td>{formatDuration(item.medianQueueWaitMs)}</td>
      <td>
        {item.lastBuild?.endedAt ? (
          age(item.lastBuild.endedAt)
        ) : item.lastBuild?.startedAt ? (
          age(item.lastBuild.startedAt)
        ) : (
          <span className={styles.dim}>—</span>
        )}
      </td>
    </tr>
  );
}

/**
 * JobTree renders one table row per visible node. A folded node's
 * descendants are never rendered, so the accessibility tree matches what
 * the reader sees.
 */
export default function JobTree({
  team,
  folder,
  rows,
  openPaths,
  onToggle,
}: {
  team: string;
  /** The tenant folder prefix. Node paths are folder-relative. */
  folder: string;
  rows: JobTreeNode[];
  openPaths: ReadonlySet<string>;
  onToggle: (path: string, next: boolean) => void;
}) {
  return (
    <>
      {rows.map((node) =>
        isFoldable(node) ? (
          <FolderRow
            key={node.path}
            team={team}
            folder={folder}
            node={node}
            open={openPaths.has(node.path)}
            onToggle={onToggle}
          />
        ) : (
          <JobRow key={node.path} team={team} node={node} item={node.summary!} />
        ),
      )}
    </>
  );
}
