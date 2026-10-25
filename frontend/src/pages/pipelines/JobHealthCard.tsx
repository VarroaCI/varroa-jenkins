import { Link } from "react-router-dom";
import { Card } from "../../components/Card";
import type { JobHealth, JobTrendPoint } from "../../types";
import { HealthGlyph } from "./HealthGlyph";
import { formatDuration } from "./JobTree";
import styles from "./JobHealthCard.module.css";

const enc = (s: string) => encodeURIComponent(s);

/** Results that the health window does not count. */
const UNCOUNTED = new Set(["INTERRUPTED", "NOT_BUILT"]);

function resultClass(result: string): string {
  if (UNCOUNTED.has(result)) return styles.resultUncounted;
  switch (result) {
    case "SUCCESS":
      return styles.resultPassed;
    case "UNSTABLE":
      return styles.resultUnstable;
    default:
      return styles.resultFailed;
  }
}

function pointName(point: JobTrendPoint): string {
  return point.buildNumber !== undefined ? `#${point.buildNumber}` : "Run";
}

function pointLabel(point: JobTrendPoint): string {
  const note = UNCOUNTED.has(point.result) ? ", not counted" : "";
  return `${pointName(point)} ${point.result}${note}`;
}

/** ResultStrip draws one square per run, oldest on the left. Each square
 * links to its build page. */
function ResultStrip({ tenant, jobPath, points }: { tenant: string; jobPath: string; points: JobTrendPoint[] }) {
  return (
    <ol className={styles.strip} aria-label="Recent results, oldest first">
      {points.map((point) => (
        <li key={point.runId}>
          <Link
            className={`${styles.square} ${resultClass(point.result)}`}
            to={`/pipelines/${enc(tenant)}/${enc(jobPath)}/${enc(point.runId)}`}
            aria-label={pointLabel(point)}
            title={pointLabel(point)}
          />
        </li>
      ))}
    </ol>
  );
}

/** DurationChart draws one bar per run with a duration, oldest on the left.
 * The tallest bar is the longest run in the window. */
function DurationChart({ points }: { points: JobTrendPoint[] }) {
  const width = 300;
  const height = 80;
  const durations = points.map((p) => p.durationMs ?? 0);
  const max = Math.max(...durations, 1);
  const slot = width / Math.max(points.length, 1);
  const longest = Math.max(...durations, 0);
  return (
    <svg
      className={styles.chart}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      role="img"
      aria-label={`Build durations, oldest first. Longest ${formatDuration(longest)}.`}
    >
      {points.map((point, i) => {
        if (point.durationMs === undefined) return null;
        const h = (point.durationMs / max) * height;
        const label = `${pointName(point)} · ${formatDuration(point.durationMs)} · ${point.result}`;
        return (
          <rect
            key={point.runId}
            className={styles.durationBar}
            x={i * slot + slot * 0.15}
            y={height - h}
            width={slot * 0.7}
            height={h}
          >
            <title>{label}</title>
          </rect>
        );
      })}
    </svg>
  );
}

/** TestChart draws one stacked bar per run that reported suite counters:
 * passed, then failed, then skipped. The tallest bar is the largest total. */
function TestChart({ points }: { points: JobTrendPoint[] }) {
  const width = 300;
  const height = 80;
  const totals = points.map((p) => (p.tests ? p.tests.passed + p.tests.failed + p.tests.skipped : 0));
  const max = Math.max(...totals, 1);
  const slot = width / Math.max(points.length, 1);
  return (
    <svg
      className={styles.chart}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      role="img"
      aria-label="Test counts per build, oldest first: passed, failed, and skipped."
    >
      {points.map((point, i) => {
        if (!point.tests) return null;
        const scale = height / max;
        const x = i * slot + slot * 0.15;
        const w = slot * 0.7;
        const segments = [
          { cls: styles.testPassed, value: point.tests.passed },
          { cls: styles.testFailed, value: point.tests.failed },
          { cls: styles.testSkipped, value: point.tests.skipped },
        ];
        let bottom = height;
        const label = `${pointName(point)} · ${point.tests.passed} passed, ${point.tests.failed} failed, ${point.tests.skipped} skipped`;
        return (
          <g key={point.runId}>
            <title>{label}</title>
            {segments.map((seg, s) => {
              const h = seg.value * scale;
              bottom -= h;
              return h > 0 ? <rect key={s} className={seg.cls} x={x} y={bottom} width={w} height={h} /> : null;
            })}
          </g>
        );
      })}
    </svg>
  );
}

/**
 * JobHealthCard shows one job's health: the weather glyph and its counts,
 * the result strip, the duration chart, and the test chart. The charts are
 * inline SVG. The test chart appears only when a run reported suite counters.
 */
export function JobHealthCard({
  tenant,
  jobPath,
  health,
  trend,
}: {
  tenant: string;
  jobPath: string;
  health: JobHealth;
  trend: JobTrendPoint[];
}) {
  const points = [...trend].reverse();
  const hasTests = points.some((p) => p.tests !== undefined);
  const hasDurations = points.some((p) => p.durationMs !== undefined);
  return (
    <Card title="Health">
      {health.bucket === "unknown" ? (
        <p className={styles.summary}>No counted build yet</p>
      ) : (
        <div className={styles.summary}>
          <HealthGlyph health={health} />
          <span>
            {health.passed} of {health.counted} builds passed
            {health.score !== undefined && ` · ${health.score}%`}
          </span>
        </div>
      )}
      {points.length > 0 && (
        <>
          <h3 className={styles.heading}>Results</h3>
          <ResultStrip tenant={tenant} jobPath={jobPath} points={points} />
          {hasDurations && (
            <>
              <h3 className={styles.heading}>Duration</h3>
              <DurationChart points={points} />
            </>
          )}
          {hasTests && (
            <>
              <h3 className={styles.heading}>Tests</h3>
              <TestChart points={points} />
            </>
          )}
        </>
      )}
    </Card>
  );
}
