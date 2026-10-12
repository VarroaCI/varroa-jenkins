import { useCallback, useEffect, useState } from "react";
import { useParams, useNavigate, useSearchParams, Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getPipelineJob, listRuns, teamRunStreamUrl, type ListRunsParams } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useEventStream } from "../../hooks/useEventStream";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { usePermissions, canAccessTenantPipeline } from "../../hooks/usePermissions";
import { age } from "../../components/activityTimeline.util";
import { stripTenantFolder } from "./jobPath";
import { jobTitle } from "./pageTitles";
import JobBreadcrumbs from "./JobBreadcrumbs";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { RunDrawer } from "./RunDrawer";
import { MultibranchCard } from "./MultibranchCard";
import { matchForgeShape, repositoryHref, fileHref } from "./forgeLink";
import { RevisionLink } from "./RevisionLink";
import { TRIGGER_LABEL, changeRequestLabel, queueWaitMs, safeWebUrl } from "./runMeta";
import type {
  ChangeRequest,
  RunNotification,
  PipelineTemplateInfo,
  PipelineDependencyEdge,
  PipelineDefinition,
  JobDeclaredBy,
} from "../../types";
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

/** PrChip marks a run that builds a pull request. Its visible text is also its
 * accessible name, so the label stays in the name. */
function PrChip({ changeRequest }: { changeRequest: ChangeRequest }) {
  const title = changeRequest.title ? ` ${changeRequest.title}` : "";
  const label = `PR #${changeRequest.id}${title}`;
  const href = safeWebUrl(changeRequest.url);
  if (href) {
    return (
      <a href={href} target="_blank" rel="noreferrer" className={styles.prChip} title={changeRequestLabel(changeRequest)}>
        {label}
      </a>
    );
  }
  return (
    <span className={styles.prChip} title={changeRequestLabel(changeRequest)}>
      {label}
    </span>
  );
}

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

/**
 * jobPageHref links the job page at its folder-qualified path. The
 * route takes the whole path, so the tenant folder stays in it.
 */
function jobPageHref(tenant: string, jobPath: string): string {
  return `/pipelines/${enc(tenant)}/${enc(jobPath)}`;
}

/**
 * renderDependencyRow renders one direction of the Triggers card's
 * dependency edges: a dt/dd pair naming each edge as tenant/job with its
 * result threshold, and "grant missing" for an edge whose grant is
 * false. Each link takes the edge's jobPath. It renders nothing for an
 * empty edge list, so the row does not appear at all.
 */
function renderDependencyRow(label: string, edges: PipelineDependencyEdge[]) {
  if (edges.length === 0) return null;
  return (
    <>
      <dt>{label}</dt>
      <dd>
        {edges.map((edge) => (
          <div key={`${edge.tenant}/${edge.job}`}>
            <Link to={jobPageHref(edge.tenant, edge.jobPath)}>
              {edge.tenant}/{edge.job}
            </Link>{" "}
            ({edge.result}
            {!edge.granted && <span className={styles.grantMissing}> · grant missing</span>})
            {edge.lastRun && (
              <>
                {" "}· last run{" "}
                {edge.lastRun.runId && edge.lastRun.jobPath ? (
                  <Link to={runHref(edge.lastRun.tenant, edge.lastRun.jobPath, edge.lastRun.runId)}>
                    {edge.lastRun.buildNumber != null ? `#${edge.lastRun.buildNumber}` : "view"}
                  </Link>
                ) : (
                  <>{edge.lastRun.buildNumber != null ? `#${edge.lastRun.buildNumber}` : edge.lastRun.tenant}</>
                )}
              </>
            )}
          </div>
        ))}
      </dd>
    </>
  );
}

/**
 * renderJenkinsfileRow renders the "Defined by" card's Jenkinsfile row
 * value. The row links to the file when four conditions hold: the
 * definition source is "jenkinsfile", the repository matches a
 * recognized forge shape, the commit is present, and the script path is
 * present. Every other case prints the repository and, when present, the
 * short commit as plain text, with no link. The text and the truncation
 * do not change.
 */
function renderJenkinsfileRow(definition: PipelineDefinition) {
  const { jenkinsfileRepo, jenkinsfileCommit, definitionPath } = definition;
  if (!jenkinsfileRepo) return null;
  const match = matchForgeShape(jenkinsfileRepo);
  const text = (
    <>
      {jenkinsfileRepo}
      {jenkinsfileCommit ? ` @ ${jenkinsfileCommit.slice(0, 7)}` : ""}
    </>
  );
  if (definition.source !== "jenkinsfile" || !match || !jenkinsfileCommit || !definitionPath) {
    return text;
  }
  return (
    <a href={fileHref(match, jenkinsfileCommit, definitionPath)} target="_blank" rel="noreferrer">
      {text}
    </a>
  );
}

/**
 * renderSourceInputRow renders the "Defined by" card's Source input row
 * value. A catalog item input prints the item name and the namespace as
 * text. A git or OCI input prints the URL and, when present, the first 12
 * characters of the revision. The row links to the repository only when
 * the URL matches a recognized forge shape and the revision is present.
 */
function renderSourceInputRow(declaredBy: JobDeclaredBy) {
  if (declaredBy.kind === "itemRef") {
    // catalogItem is optional in the schema, even for kind itemRef: the
    // required list holds only kind, name, and bundle. Fall back to the
    // name field instead of asserting the value.
    if (!declaredBy.catalogItem) return declaredBy.name;
    return `${declaredBy.catalogItem.name} (${declaredBy.catalogItem.namespace})`;
  }
  const url = declaredBy.kind === "gitSource" ? declaredBy.repoURL : declaredBy.ociRef;
  if (!url) return declaredBy.name;
  const match = declaredBy.kind === "gitSource" ? matchForgeShape(url) : null;
  const revisionText = declaredBy.revision ? ` @ ${declaredBy.revision.slice(0, 12)}` : "";
  if (match && declaredBy.revision) {
    return (
      <a href={repositoryHref(match, declaredBy.revision)} target="_blank" rel="noreferrer">
        {url}
        {revisionText}
      </a>
    );
  }
  return `${url}${revisionText}`;
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

  useDocumentTitle(
    jobQuery.data && team ? jobTitle(team, jobQuery.data.tenantFolder, jobQuery.data.jobPath) : null,
  );

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

  // The team's stream carries every job of that team, so the page keeps the
  // frames of its own job only.
  const { lastEvent, readyState } = useEventStream<RunNotification>(
    team ? teamRunStreamUrl(team) : null,
    team ? `pipelines:${team}` : null,
    ["run", "stage"],
  );
  const queryClient = useQueryClient();

  // The runs table carries the filter the URL holds, so the key repeats
  // that filter to match the query's own key.
  const invalidateRuns = useCallback(() => {
    void queryClient.invalidateQueries({
      queryKey: [
        "pipelineRuns",
        team,
        job,
        {
          branch: branch || undefined,
          result: result || undefined,
          actor: actor || undefined,
          cause: cause || undefined,
        },
      ],
    });
  }, [queryClient, team, job, branch, result, actor, cause]);

  const invalidateJob = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["pipelineJob", team, job] });
  }, [queryClient, team, job]);

  // The stream replays nothing, so an open stream is the page's signal to
  // refetch both reads.
  useEffect(() => {
    if (readyState !== "open") return;
    invalidateRuns();
    invalidateJob();
  }, [readyState, invalidateRuns, invalidateJob]);

  useEffect(() => {
    // The page drops a stage frame: the table and the job read carry no
    // stage value.
    if (lastEvent?.type !== "run") return;
    // The page matches the job path the URL holds, and renders no payload
    // field.
    if (lastEvent.data.job_path !== job) return;
    invalidateRuns();
    // A terminal run moves the health values and the Last 30 days card.
    if (lastEvent.data.status === "COMPLETED") invalidateJob();
  }, [lastEvent, job, invalidateRuns, invalidateJob]);

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
  // One local keeps the narrowing the row guards establish.
  const definition = jobDetail.definition;
  const declaredBy = definition?.declaredBy;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>{jobDetail.jobPath}</div>
          <JobBreadcrumbs team={team!} tenantFolder={jobDetail.tenantFolder} jobPath={jobDetail.jobPath} />
        </div>
        {canRun && jobDetail.kind !== "multibranch" && (
          <button type="button" className={styles.runBtn} onClick={() => setDrawerOpen(true)}>
            Run
          </button>
        )}
      </div>

      <div className={styles.cardGrid}>
        <Card title="Defined by">
          <dl className={styles.kv}>
            {definition?.jenkinsfileRepo && (
              <>
                <dt>Jenkinsfile</dt>
                <dd>{renderJenkinsfileRow(definition)}</dd>
              </>
            )}
            {definition?.source === "inline" && (
              <>
                <dt>Script</dt>
                <dd>Inline, declared by the bundle item.</dd>
              </>
            )}
            {jobDetail.template && (
              <>
                <dt>Template</dt>
                <dd>{renderTemplateRow(jobDetail.template)}</dd>
              </>
            )}
            {definition?.profile && (
              <>
                <dt>Profile</dt>
                <dd>{definition.profile}</dd>
              </>
            )}
            {declaredBy && (
              <>
                <dt>Bundle</dt>
                <dd>
                  <Link to={`/catalog/bundles/${enc(declaredBy.bundle.namespace)}/${enc(declaredBy.bundle.name)}`}>
                    {declaredBy.bundle.name}
                  </Link>
                </dd>
                <dt>Source input</dt>
                <dd>{renderSourceInputRow(declaredBy)}</dd>
              </>
            )}
            {!jobDetail.definition && !jobDetail.template && (
              <dd className={styles.dim}>Not available.</dd>
            )}
          </dl>
        </Card>

        {jobDetail.waiting && jobDetail.waiting.length > 0 && (
          <Card title="Waiting">
            <dl className={styles.kv}>
              {jobDetail.waiting.map((w) => (
                <div key={w.runId}>
                  <dt>{w.kind === "cell" ? "Cell cap" : w.limit ?? "Limit"}</dt>
                  <dd>
                    {w.running} of {w.max} builds run, waiting since {new Date(w.since).toLocaleTimeString()}.{" "}
                    <span>Run {w.runId}</span>
                  </dd>
                </div>
              ))}
            </dl>
          </Card>
        )}

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
            <dt>Median queue wait</dt>
            <dd>
              {!jobDetail.last30Days ? (
                <span className={styles.dim}>Not available</span>
              ) : jobDetail.last30Days.medianQueueWaitMs !== undefined ? (
                formatDuration(jobDetail.last30Days.medianQueueWaitMs)
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

      {jobDetail.kind === "multibranch" ? (
        <MultibranchCard team={team!} jobPath={jobDetail.jobPath} />
      ) : runsQuery.isLoading ? (
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
                <th>Queue</th>
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
                  <td>
                    {run.branch ? (
                      <Link to={`/pipelines/${enc(team!)}/${enc(job!)}?branch=${enc(run.branch)}`}>{run.branch}</Link>
                    ) : (
                      <span className={styles.dim}>—</span>
                    )}
                    {run.changeRequest && <PrChip changeRequest={run.changeRequest} />}
                  </td>
                  <td>
                    {run.scmRevision ? (
                      <RevisionLink revision={run.scmRevision} repoUrl={run.scmUrl} length={7} />
                    ) : (
                      <span className={styles.dim}>—</span>
                    )}
                  </td>
                  <td title={run.triggerSource ? `Triggered by ${TRIGGER_LABEL[run.triggerSource]}` : undefined}>
                    {run.cause ?? <span className={styles.dim}>—</span>}
                  </td>
                  <td>{formatDuration(run.durationMs)}</td>
                  <td>{formatDuration(queueWaitMs(run))}</td>
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
