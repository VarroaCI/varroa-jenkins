import { Link } from "react-router-dom";
import { Card } from "../../components/Card";
import type { CoverageReport, QualityCounter, RunQuality, WarningsReport } from "../../types";
import styles from "./QualityCard.module.css";

const DASH = "—";

const enc = (s: string) => encodeURIComponent(s);

const runHref = (tenant: string, jobPath: string, runId: string) =>
  `/pipelines/${enc(tenant)}/${enc(jobPath)}/${enc(runId)}`;

// formatPercent shows the percent that the API sends. It never computes one.
function formatPercent(c?: QualityCounter): string {
  return c?.percent === undefined ? DASH : `${c.percent.toFixed(1)}%`;
}

function signedPoints(v: number): string {
  return v > 0 ? `+${v.toFixed(1)}` : v.toFixed(1);
}

function signedCount(v: number): string {
  return v > 0 ? `+${v}` : String(v);
}

// droppedNote names the reports Varroa did not keep. The bound of 20 matches
// the API cap, which the plugin also applies.
function droppedNote(count: number, noun: string): string {
  return `Jenkins published ${count} more ${noun}${count === 1 ? "" : "s"}. Varroa keeps the first 20.`;
}

function gateClass(status: string): string {
  switch (status) {
    case "PASSED":
      return styles.gatePassed;
    case "WARNING":
    case "UNSTABLE":
    case "NOTE":
      return styles.gateWarning;
    case "FAILED":
    case "ERROR":
      return styles.gateFailed;
    default:
      return styles.gateOther;
  }
}

function Gate({ status }: { status?: string }) {
  if (!status) return <>{DASH}</>;
  return <span className={gateClass(status)}>{status}</span>;
}

function BaseLink({ tenant, jobPath, baseRunId, baseBuildNumber }: {
  tenant: string;
  jobPath: string;
  baseRunId: string;
  baseBuildNumber?: number;
}) {
  const label = baseBuildNumber !== undefined ? `#${baseBuildNumber}` : "an earlier build";
  return <Link to={runHref(tenant, jobPath, baseRunId)}>{label}</Link>;
}

function CoverageTable({ reports, tenant, jobPath }: {
  reports: CoverageReport[];
  tenant: string;
  jobPath: string;
}) {
  const showModified = reports.some((r) => r.modifiedLines);
  return (
    <div className={styles.tableScroll}>
      <table className={styles.table} aria-label="Coverage reports">
        <thead>
          <tr>
            <th scope="col">Report</th>
            <th scope="col">Line</th>
            <th scope="col">Branch</th>
            {showModified && <th scope="col">Modified lines</th>}
            <th scope="col">Change</th>
            <th scope="col">Gate</th>
          </tr>
        </thead>
        <tbody>
          {reports.map((r) => {
            const parts: string[] = [];
            if (r.delta?.linePoints !== undefined) parts.push(`line ${signedPoints(r.delta.linePoints)} pts`);
            if (r.delta?.branchPoints !== undefined) parts.push(`branch ${signedPoints(r.delta.branchPoints)} pts`);
            return (
              <tr key={r.id}>
                <td>{r.name}</td>
                <td title={r.line ? `${r.line.covered} of ${r.line.total}` : undefined}>{formatPercent(r.line)}</td>
                <td title={r.branch ? `${r.branch.covered} of ${r.branch.total}` : undefined}>{formatPercent(r.branch)}</td>
                {showModified && (
                  <td title={r.modifiedLines ? `${r.modifiedLines.covered} of ${r.modifiedLines.total}` : undefined}>
                    {formatPercent(r.modifiedLines)}
                  </td>
                )}
                <td>
                  {r.delta ? (
                    <>
                      {parts.join(", ") || "no change data"} vs{" "}
                      <BaseLink tenant={tenant} jobPath={jobPath} baseRunId={r.delta.baseRunId} baseBuildNumber={r.delta.baseBuildNumber} />
                    </>
                  ) : DASH}
                </td>
                <td><Gate status={r.gateStatus} /></td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function WarningsTable({ reports, tenant, jobPath }: {
  reports: WarningsReport[];
  tenant: string;
  jobPath: string;
}) {
  const showNewFixed = reports.some((r) => r.new !== undefined || r.fixed !== undefined);
  return (
    <div className={styles.tableScroll}>
      <table className={styles.table} aria-label="Warnings reports">
        <thead>
          <tr>
            <th scope="col">Report</th>
            <th scope="col">Total</th>
            <th scope="col">Error</th>
            <th scope="col">High</th>
            <th scope="col">Normal</th>
            <th scope="col">Low</th>
            {showNewFixed && <th scope="col">New</th>}
            {showNewFixed && <th scope="col">Fixed</th>}
            <th scope="col">Change</th>
            <th scope="col">Gate</th>
          </tr>
        </thead>
        <tbody>
          {reports.map((r) => (
            <tr key={r.id}>
              <td>{r.name}</td>
              <td>{r.total}</td>
              <td>{r.error}</td>
              <td>{r.high}</td>
              <td>{r.normal}</td>
              <td>{r.low}</td>
              {showNewFixed && <td>{r.new ?? DASH}</td>}
              {showNewFixed && <td>{r.fixed ?? DASH}</td>}
              <td>
                {r.delta ? (
                  <>
                    {signedCount(r.delta.total)} vs{" "}
                    <BaseLink tenant={tenant} jobPath={jobPath} baseRunId={r.delta.baseRunId} baseBuildNumber={r.delta.baseBuildNumber} />
                  </>
                ) : DASH}
              </td>
              <td><Gate status={r.gateStatus} /></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** QualityCard shows a run's coverage and warnings reports. It renders report
 * names as text. It shows the percent values that the API sends and computes
 * none. */
export function QualityCard({ quality, tenant, jobPath }: {
  quality?: RunQuality;
  tenant: string;
  jobPath: string;
}) {
  const coverage = quality?.coverage ?? [];
  const warnings = quality?.warnings ?? [];
  if (coverage.length === 0 && warnings.length === 0) return null;
  const coverageDropped = quality?.coverageDropped ?? 0;
  const warningsDropped = quality?.warningsDropped ?? 0;
  return (
    <Card title="Quality">
      {coverage.length > 0 && (
        <>
          <h3 className={styles.heading}>Coverage</h3>
          <CoverageTable reports={coverage} tenant={tenant} jobPath={jobPath} />
          {coverageDropped > 0 && (
            <p className={styles.note}>{droppedNote(coverageDropped, "coverage report")}</p>
          )}
        </>
      )}
      {warnings.length > 0 && (
        <>
          <h3 className={styles.heading}>Warnings</h3>
          <WarningsTable reports={warnings} tenant={tenant} jobPath={jobPath} />
          {warningsDropped > 0 && (
            <p className={styles.note}>{droppedNote(warningsDropped, "warnings report")}</p>
          )}
        </>
      )}
    </Card>
  );
}
