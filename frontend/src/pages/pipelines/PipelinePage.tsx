import { useState } from "react";
import { useParams, useNavigate, useSearchParams, Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { getPipelineJob, listRuns, type ListRunsParams } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { usePermissions, canAccessTenantPipeline } from "../../hooks/usePermissions";
import { age } from "../../components/activityTimeline.util";
import { stripTenantFolder } from "./jobPath";
import { RunDrawer } from "./RunDrawer";
import type { PipelineTemplateInfo, PipelineDependencyEdge } from "../../types";
import styles from "./PipelinePage.module.css";

const enc = (s: string) => encodeURIComponent(s);

function runHref(team: string, jobPath: string, runId: string): string {
  return `/pipelines/${enc(team)}/${enc(jobPath)}/${enc(runId)}`;
}

/** formatDuration renders a millisecond span as the two coarsest non-zero
 * units, dropping finer ones. */
function formatDuration(ms: number | undefined): string {
  if (ms === undefined || ms < 0) return "—";
  const totalSec = Math.round(ms / 1000);
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

const RESULT_CLASS: Record<string, string> = {
  SUCCESS: styles.resultOk,
  FAILURE: styles.resultBad,
  UNSTABLE: styles.resultWarn,
  ABORTED: styles.resultIdle,
  INTERRUPTED: styles.resultBad,
  NOT_BUILT: styles.resultIdle,
};

function ResultBadge({ result }: { result?: string }) {
  if (!result) return <span className={styles.dim}>—</span>;
  const cls = RESULT_CLASS[result] ?? styles.resultIdle;
  return <span className={`${styles.resultBadge} ${cls}`}>{result}</span>;
}

/**
 * renderTemplateRow renders the "Defined by" card's Template row for the
 * three cases where PipelineJobDetail.template is present. An
 * items-declared job (source "items") shows the name and version. A
 * marker-defined job with a run shows the name, the version, the marker
 * path, and the build the values came from, because a marker changes
 * between builds. A marker-defined job with no run yet shows the marker
 * path and states that the template resolves at the first build. The
 * fourth case — no template object at all, for a Jenkinsfile job with no
 * catalog template — is handled by the caller, which renders no row.
 */
function renderTemplateRow(template: PipelineTemplateInfo) {
  if (template.source === "items") {
    return (
      <>
        {template.name}
        {template.version ? ` v${template.version}` : ""}
      </>
    );
  }

  if (!template.name) {
    return (
      <>
        {template.markerPath}
        <div className={styles.dim}>Resolves at the first build.</div>
      </>
    );
  }

  return (
    <>
      {template.name}
      {template.version ? ` v${template.version}` : ""}
      {template.markerPath ? ` · ${template.markerPath}` : ""}
      {template.asOfBuild !== undefined ? ` · build ${template.asOfBuild}` : ""}
    </>
  );
}

function jobPageHref(tenant: string, job: string): string {
  return `/pipelines/${enc(tenant)}/${enc(job)}`;
}

/**
 * renderDependencyRow renders one direction of the Triggers card's
 * dependency edges: a dt/dd pair naming each edge as tenant/job with its
 * result threshold, and "grant missing" for an edge whose grant is
 * false. It renders nothing for an empty edge list, so the row does not
 * appear at all.
 */
function renderDependencyRow(label: string, edges: PipelineDependencyEdge[]) {
  if (edges.length === 0) return null;
  return (
    <>
      <dt>{label}</dt>
      <dd>
        {edges.map((edge) => (
          <div key={`${edge.tenant}/${edge.job}`}>
            <Link to={jobPageHref(edge.tenant, edge.job)}>
              {edge.tenant}/{edge.job}
            </Link>{" "}
            ({edge.result}
            {!edge.granted && <span className={styles.grantMissing}> · grant missing</span>})
          </div>
        ))}
      </dd>
    </>
  );
}

export default function PipelinePage() {
  const { team, job } = useParams<{ team: string; job: string }>();
  const navigate = useNavigate();
  const { data: permissions } = usePermissions();
  const [searchParams, setSearchParams] = useSearchParams();
  const [drawerOpen, setDrawerOpen] = useState(false);

  const branch = searchParams.get("branch") ?? "";
  const result = searchParams.get("result") ?? "";
  const actor = searchParams.get("actor") ?? "";
  const cause = searchParams.get("cause") ?? "";

  const setFilter = (key: string, value: string) =>
    setSearchParams((current) => {
      const next = new URLSearchParams(current);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  const jobQuery = useQuery({
    queryKey: ["pipelineJob", team, job],
    queryFn: () => getPipelineJob(team!, job!),
    enabled: !!team && !!job,
  });

  const runsFilter: ListRunsParams = {
    branch: branch || undefined,
    result: result || undefined,
    actor: actor || undefined,
    cause: cause || undefined,
  };
  const runsQuery = useQuery({
    queryKey: ["pipelineRuns", team, job, runsFilter],
    queryFn: () => listRuns(team!, job!, runsFilter),
    enabled: !!team && !!job && !jobQuery.error,
  });

  if (jobQuery.isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading pipeline…</div>
      </div>
    );
  }

  const jobError = jobQuery.error;
  if (jobError instanceof ApiError && jobError.status === 403) return <ForbiddenPage />;
  if (jobError instanceof ApiError && jobError.status === 404) return <NotFoundPage />;
  if (jobError instanceof ApiError && jobError.status === 503) return <RecordStoreDisabledPage />;
  if (jobError) return <GenericErrorPage />;
  const jobDetail = jobQuery.data;
  if (!jobDetail) return <NotFoundPage />;

  const folderRelativeJobPath = stripTenantFolder(jobDetail.tenantFolder, jobDetail.jobPath);
  const canRun = canAccessTenantPipeline(permissions, jobDetail.tenant, "write", folderRelativeJobPath);
  const runs = runsQuery.data?.items ?? [];

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>{jobDetail.jobPath}</div>
          <div className={styles.pageDesc}>{team}</div>
        </div>
        {canRun && (
          <button type="button" className={styles.runBtn} onClick={() => setDrawerOpen(true)}>
            Run
          </button>
        )}
      </div>

      <div className={styles.cardGrid}>
        <Card title="Defined by">
          <dl className={styles.kv}>
            {jobDetail.definition?.jenkinsfileRepo && (
              <>
                <dt>Jenkinsfile</dt>
                <dd>
                  {jobDetail.definition.jenkinsfileRepo}
                  {jobDetail.definition.jenkinsfileCommit ? ` @ ${jobDetail.definition.jenkinsfileCommit.slice(0, 7)}` : ""}
                </dd>
              </>
            )}
            {jobDetail.template && (
              <>
                <dt>Template</dt>
                <dd>{renderTemplateRow(jobDetail.template)}</dd>
              </>
            )}
            {jobDetail.definition?.profile && (
              <>
                <dt>Profile</dt>
                <dd>{jobDetail.definition.profile}</dd>
              </>
            )}
            {!jobDetail.definition && <dd className={styles.dim}>Not available.</dd>}
          </dl>
        </Card>

        <Card title="Triggers">
          <dl className={styles.kv}>
            <dt>Push</dt>
            <dd>{jobDetail.triggers?.push ? "Yes" : "No"}</dd>
            <dt>Pull request</dt>
            <dd>{jobDetail.triggers?.pullRequest ? "Yes" : "No"}</dd>
            <dt>Schedule</dt>
            <dd>{jobDetail.triggers?.schedule ?? <span className={styles.dim}>—</span>}</dd>
            <dt>Concurrency</dt>
            <dd>{jobDetail.triggers?.concurrency ?? <span className={styles.dim}>—</span>}</dd>
            {renderDependencyRow("Upstream", jobDetail.dependencies.upstream)}
            {renderDependencyRow("Downstream", jobDetail.dependencies.downstream)}
          </dl>
        </Card>

        {/* last30Days is null when the record store could not answer the
            window query; "Not available" then replaces every row, rather
            than a per-row dash that would read as "zero". */}
        <Card title="Last 30 days">
          <dl className={styles.kv}>
            <dt>Builds</dt>
            <dd>
              {jobDetail.last30Days ? jobDetail.last30Days.builds : <span className={styles.dim}>Not available</span>}
            </dd>
            <dt>Pass rate</dt>
            <dd>
              {!jobDetail.last30Days ? (
                <span className={styles.dim}>Not available</span>
              ) : jobDetail.last30Days.passRate !== undefined ? (
                `${Math.round(jobDetail.last30Days.passRate * 100)}%`
              ) : (
                <span className={styles.dim}>—</span>
              )}
            </dd>
            <dt>Median</dt>
            <dd>
              {!jobDetail.last30Days ? (
                <span className={styles.dim}>Not available</span>
              ) : jobDetail.last30Days.medianDurationMs !== undefined ? (
                formatDuration(jobDetail.last30Days.medianDurationMs)
              ) : (
                <span className={styles.dim}>—</span>
              )}
            </dd>
            <dt>Slowest stage</dt>
            <dd>
              {!jobDetail.last30Days ? (
                <span className={styles.dim}>Not available</span>
              ) : jobDetail.last30Days.slowestStage ? (
                `${jobDetail.last30Days.slowestStage.name} (${formatDuration(jobDetail.last30Days.slowestStage.durationMs)})`
              ) : (
                <span className={styles.dim}>—</span>
              )}
            </dd>
          </dl>
        </Card>
      </div>

      <div className={styles.sectionTitle}>Builds</div>
      <div className={styles.filterBar}>
        <input
          className={styles.filterInput}
          placeholder="Branch"
          aria-label="Branch filter"
          value={branch}
          onChange={(e) => setFilter("branch", e.target.value)}
        />
        <input
          className={styles.filterInput}
          placeholder="Result"
          aria-label="Result filter"
          value={result}
          onChange={(e) => setFilter("result", e.target.value)}
        />
        <input
          className={styles.filterInput}
          placeholder="Actor"
          aria-label="Actor filter"
          value={actor}
          onChange={(e) => setFilter("actor", e.target.value)}
        />
        <input
          className={styles.filterInput}
          placeholder="Cause"
          aria-label="Cause filter"
          value={cause}
          onChange={(e) => setFilter("cause", e.target.value)}
        />
      </div>

      {runsQuery.isLoading ? (
        <div className={styles.loading}>Loading builds…</div>
      ) : runs.length === 0 ? (
        <div className={styles.empty}>No builds match this filter.</div>
      ) : (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Build</th>
                <th>Result</th>
                <th>Branch</th>
                <th>Commit</th>
                <th>Cause</th>
                <th>Duration</th>
                <th>When</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((run) => (
                <tr key={run.runId}>
                  <td className={styles.nameCell}>
                    <Link to={runHref(team!, job!, run.runId)}>#{run.buildNumber ?? "—"}</Link>
                  </td>
                  <td>
                    <ResultBadge result={run.result} />
                    {run.result === "INTERRUPTED" && <div className={styles.interruptedNote}>Interrupted</div>}
                  </td>
                  <td>{run.branch ?? <span className={styles.dim}>—</span>}</td>
                  <td>{run.scmRevision ? run.scmRevision.slice(0, 7) : <span className={styles.dim}>—</span>}</td>
                  <td>{run.cause ?? <span className={styles.dim}>—</span>}</td>
                  <td>{formatDuration(run.durationMs)}</td>
                  <td>
                    {run.endedAt ? age(run.endedAt) : run.startedAt ? age(run.startedAt) : <span className={styles.dim}>—</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}

      {drawerOpen && (
        <RunDrawer
          team={team!}
          jobPath={job!}
          parameters={jobDetail.parameters ?? []}
          onClose={() => setDrawerOpen(false)}
          onSubmitted={(runId) => {
            setDrawerOpen(false);
            navigate(runHref(team!, job!, runId), { state: { requested: true } });
          }}
        />
      )}
    </div>
  );
}
