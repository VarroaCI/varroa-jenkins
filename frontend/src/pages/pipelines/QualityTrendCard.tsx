import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { Card } from "../../components/Card";
import type {
  CoverageTrendPoint,
  CoverageTrendSeries,
  JobTrendPoint,
  QualityTrend,
  WarningsTrendPoint,
  WarningsTrendSeries,
} from "../../types";
import styles from "./QualityTrendCard.module.css";

// The chart box. The viewBox keeps these units; CSS scales the box to the card.
const W = 300;
const H = 80;
const PAD = 4;

const enc = (s: string) => encodeURIComponent(s);

const runHref = (tenant: string, jobPath: string, runId: string) =>
  `/pipelines/${enc(tenant)}/${enc(jobPath)}/${enc(runId)}`;

const buildName = (p: { buildNumber?: number }) => (p.buildNumber !== undefined ? `#${p.buildNumber}` : "Run");

interface Slot<P> {
  runId: string;
  point?: P;
}

// slotsFor lays the health window out oldest first. A point outside the
// window goes after the window slots, so no point is lost.
function slotsFor<P extends { runId: string }>(window: JobTrendPoint[], points: P[]): Slot<P>[] {
  const byRun = new Map(points.map((p) => [p.runId, p]));
  const ids = [...window].reverse().map((w) => w.runId);
  const known = new Set(ids);
  for (const p of [...points].reverse()) {
    if (!known.has(p.runId)) {
      ids.push(p.runId);
      known.add(p.runId);
    }
  }
  return ids.map((runId) => ({ runId, point: byRun.get(runId) }));
}

// geometry maps a slot index to its x centre and a percent to its y.
function geometry(n: number) {
  const slot = W / Math.max(n, 1);
  return {
    slot,
    cx: (i: number) => i * slot + slot / 2,
    y: (percent: number) => PAD + ((100 - percent) / 100) * (H - 2 * PAD),
  };
}

// segments returns one point list per run of consecutive defined values.
// A missing slot or a missing value breaks the line, which keeps the gap visible.
function segments<P>(slots: Slot<P>[], cx: (i: number) => number, y: (v: number) => number, get: (p: P) => number | undefined): string[] {
  const out: string[][] = [];
  let cur: string[] = [];
  slots.forEach((s, i) => {
    const v = s.point === undefined ? undefined : get(s.point);
    if (v === undefined) {
      if (cur.length > 0) out.push(cur);
      cur = [];
      return;
    }
    cur.push(`${cx(i)},${y(v)}`);
  });
  if (cur.length > 0) out.push(cur);
  return out.filter((seg) => seg.length >= 2).map((seg) => seg.join(" "));
}

function PointLink({ tenant, jobPath, runId, label, x, width, children }: {
  tenant: string;
  jobPath: string;
  runId: string;
  label: string;
  x: number;
  width: number;
  children?: ReactNode;
}) {
  return (
    <Link className={styles.point} to={runHref(tenant, jobPath, runId)} aria-label={label}>
      <title>{label}</title>
      <rect className={styles.hit} x={x} y={0} width={width} height={H} />
      {children}
    </Link>
  );
}

function coverageLabel(p: CoverageTrendPoint): string {
  const parts = [buildName(p)];
  if (p.linePercent !== undefined) parts.push(`line ${p.linePercent.toFixed(1)}%`);
  if (p.branchPercent !== undefined) parts.push(`branch ${p.branchPercent.toFixed(1)}%`);
  if (p.linePercent === undefined && p.branchPercent === undefined) parts.push("no percent");
  return parts.join(" · ");
}

function CoverageChart({ tenant, jobPath, series, window }: {
  tenant: string;
  jobPath: string;
  series: CoverageTrendSeries;
  window: JobTrendPoint[];
}) {
  const slots = slotsFor(window, series.points);
  const { slot, cx, y } = geometry(slots.length);
  const hasBranch = series.points.some((p) => p.branchPercent !== undefined);
  const lineSegs = segments(slots, cx, y, (p) => p.linePercent);
  const branchSegs = hasBranch ? segments(slots, cx, y, (p) => p.branchPercent) : [];
  return (
    <>
      <p className={styles.legend}>
        <span className={styles.swatchLine} aria-hidden="true" />
        Line
        {hasBranch && (
          <>
            <span className={styles.swatchBranch} aria-hidden="true" />
            Branch
          </>
        )}
      </p>
      <svg
        className={styles.chart}
        viewBox={`0 0 ${W} ${H}`}
        role="group"
        aria-label={`${series.name} coverage per build, oldest first`}
      >
        {lineSegs.map((pts, k) => (
          <polyline key={`line-${k}`} data-series="line" className={styles.lineLine} points={pts} fill="none" vectorEffect="non-scaling-stroke" />
        ))}
        {branchSegs.map((pts, k) => (
          <polyline key={`branch-${k}`} data-series="branch" className={styles.lineBranch} points={pts} fill="none" vectorEffect="non-scaling-stroke" />
        ))}
        {slots.map((s, i) => {
          if (!s.point) return null;
          const p = s.point;
          return (
            <PointLink
              key={s.runId}
              tenant={tenant}
              jobPath={jobPath}
              runId={s.runId}
              label={coverageLabel(p)}
              x={i * slot}
              width={slot}
            >
              {p.linePercent !== undefined && (
                <circle data-series="line" className={styles.dotLine} cx={cx(i)} cy={y(p.linePercent)} r={2.5} />
              )}
              {hasBranch && p.branchPercent !== undefined && (
                <circle data-series="branch" className={styles.dotBranch} cx={cx(i)} cy={y(p.branchPercent)} r={2.5} />
              )}
            </PointLink>
          );
        })}
      </svg>
    </>
  );
}

function warningsLabel(p: WarningsTrendPoint): string {
  const parts = [buildName(p), `${p.total} warnings`];
  if (p.new !== undefined) parts.push(`${p.new} new`);
  return parts.join(" · ");
}

function WarningsChart({ tenant, jobPath, series, window }: {
  tenant: string;
  jobPath: string;
  series: WarningsTrendSeries;
  window: JobTrendPoint[];
}) {
  const slots = slotsFor(window, series.points);
  const { slot } = geometry(slots.length);
  const max = Math.max(...series.points.map((p) => p.total), 1);
  return (
    <svg
      className={styles.chart}
      viewBox={`0 0 ${W} ${H}`}
      role="group"
      aria-label={`${series.name} warnings per build, oldest first`}
    >
      {slots.map((s, i) => {
        if (!s.point) return null;
        const p = s.point;
        const h = (p.total / max) * H;
        return (
          <PointLink
            key={s.runId}
            tenant={tenant}
            jobPath={jobPath}
            runId={s.runId}
            label={warningsLabel(p)}
            x={i * slot}
            width={slot}
          >
            <rect data-series="total" className={styles.bar} x={i * slot + slot * 0.15} y={H - h} width={slot * 0.7} height={h} />
          </PointLink>
        );
      })}
    </svg>
  );
}

/** QualityTrendCard draws one line chart per coverage series and one bar chart
 * per warnings series over the job's health window. Each point links to its
 * run. It renders series names as text. */
export function QualityTrendCard({ tenant, jobPath, qualityTrend, window }: {
  tenant: string;
  jobPath: string;
  qualityTrend?: QualityTrend;
  /** The job's health window, newest first, as the job read sends it. */
  window: JobTrendPoint[];
}) {
  const coverage = qualityTrend?.coverage ?? [];
  const warnings = qualityTrend?.warnings ?? [];
  if (coverage.length === 0 && warnings.length === 0) return null;
  return (
    <Card title="Quality trend">
      {coverage.map((s) => (
        <div key={`coverage-${s.id}`}>
          <h3 className={styles.heading}>{s.name} · coverage</h3>
          <CoverageChart tenant={tenant} jobPath={jobPath} series={s} window={window} />
        </div>
      ))}
      {warnings.map((s) => (
        <div key={`warnings-${s.id}`}>
          <h3 className={styles.heading}>{s.name} · warnings</h3>
          <WarningsChart tenant={tenant} jobPath={jobPath} series={s} window={window} />
        </div>
      ))}
    </Card>
  );
}
