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
import { PLOT_H, PLOT_PAD, PLOT_W, TrendPlot, endLabels } from "./TrendPlot";
import styles from "./QualityTrendCard.module.css";

// The coverage axis pads the data by PAD_PCT points on each side. Its span is
// at least MIN_SPAN points, and its bounds stay in 0..100.
const PAD_PCT = 5;
const MIN_SPAN = 10;

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

// coverageRange returns the y axis bounds for both coverage series. The axis
// covers both series. The span is at least MIN_SPAN points, and the bounds
// stay in 0..100.
function coverageRange(points: CoverageTrendPoint[], hasBranch: boolean): { lo: number; hi: number } {
  const values: number[] = [];
  for (const p of points) {
    if (p.linePercent !== undefined) values.push(p.linePercent);
    if (hasBranch && p.branchPercent !== undefined) values.push(p.branchPercent);
  }
  if (values.length === 0) return { lo: 0, hi: 100 };
  let lo = Math.max(0, Math.floor(Math.min(...values)) - PAD_PCT);
  let hi = Math.min(100, Math.ceil(Math.max(...values)) + PAD_PCT);
  lo = Math.max(0, Math.min(lo, hi - MIN_SPAN));
  hi = Math.min(100, Math.max(hi, lo + MIN_SPAN));
  return { lo, hi };
}

// geometry maps a slot index to its x centre, and a value in lo..hi to its y.
function geometry(n: number, lo = 0, hi = 100) {
  const slot = PLOT_W / Math.max(n, 1);
  return {
    slot,
    cx: (i: number) => i * slot + slot / 2,
    y: (v: number) => PLOT_PAD + ((hi - v) / (hi - lo)) * (PLOT_H - 2 * PLOT_PAD),
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
      <rect className={styles.hit} x={x} y={0} width={width} height={PLOT_H} />
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
  const hasBranch = series.points.some((p) => p.branchPercent !== undefined);
  const { lo, hi } = coverageRange(series.points, hasBranch);
  const { slot, cx, y } = geometry(slots.length, lo, hi);
  const lineSegs = segments(slots, cx, y, (p) => p.linePercent);
  const branchSegs = hasBranch ? segments(slots, cx, y, (p) => p.branchPercent) : [];
  const ends = endLabels(slots, buildName, (p) =>
    p.linePercent === undefined ? undefined : `${p.linePercent.toFixed(1)}%`,
  );
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
      <TrendPlot
        top={`${hi}%`}
        bottom={`${lo}%`}
        inset
        first={ends.first}
        last={ends.last}
        role="group"
        ariaLabel={`${series.name} coverage per build, oldest first, scale ${lo}% to ${hi}%`}
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
                <line
                  data-series="line"
                  className={styles.dotLine}
                  x1={cx(i)}
                  x2={cx(i)}
                  y1={y(p.linePercent)}
                  y2={y(p.linePercent)}
                  vectorEffect="non-scaling-stroke"
                />
              )}
              {hasBranch && p.branchPercent !== undefined && (
                <line
                  data-series="branch"
                  className={styles.dotBranch}
                  x1={cx(i)}
                  x2={cx(i)}
                  y1={y(p.branchPercent)}
                  y2={y(p.branchPercent)}
                  vectorEffect="non-scaling-stroke"
                />
              )}
            </PointLink>
          );
        })}
      </TrendPlot>
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
  const ends = endLabels(slots, buildName, (p) => String(p.total));
  return (
    <TrendPlot
      top={String(max)}
      bottom="0"
      first={ends.first}
      last={ends.last}
      role="group"
      ariaLabel={`${series.name} warnings per build, oldest first`}
    >
      {slots.map((s, i) => {
        if (!s.point) return null;
        const p = s.point;
        const h = (p.total / max) * PLOT_H;
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
            <rect data-series="total" className={styles.bar} x={i * slot + slot * 0.15} y={PLOT_H - h} width={slot * 0.7} height={h} />
          </PointLink>
        );
      })}
    </TrendPlot>
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
