import { Link, useLocation, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { getPipelineJob, getTestCaseHistory } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { Card } from "../../components/Card";
import { age } from "../../components/activityTimeline.util";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../../components/RecoveryState";
import { formatDuration } from "./JobTree";
import JobBreadcrumbs from "./JobBreadcrumbs";
import { relativeJob, testCaseTitle } from "./pageTitles";
import type { TestCaseLinkState } from "./jobPath";
import styles from "./TestCaseHistory.module.css";

/**
 * caseTitle names the class and the test. A case link passes both in its
 * route state. A direct URL carries no state, so the page shows the case
 * key as the router decodes it. The page never splits the key.
 */
function caseTitle(state: unknown, caseKey: string): { className?: string; name: string } {
  const s = state as Partial<TestCaseLinkState> | null;
  if (s && typeof s.className === "string" && typeof s.name === "string") {
    return { className: s.className, name: s.name };
  }
  return { name: caseKey };
}

/**
 * caseKeyFromPath reads the case key from the last segment of the raw URL
 * path, decoded once. The router's own param is not safe here: it turns
 * every "%2F" in a segment into "/", and a case key half can hold "%2F".
 */
export function caseKeyFromPath(pathname: string): string | undefined {
  const segments = pathname.split("/").filter((s) => s !== "");
  const last = segments[segments.length - 1];
  if (!last) return undefined;
  try {
    return decodeURIComponent(last);
  } catch {
    return undefined;
  }
}

/**
 * TestCaseHistory lists the recent outcomes of one test case of one job,
 * newest first, in the order the response carries them. It shows no rate,
 * no trend, and no chart.
 */
export default function TestCaseHistory() {
  const { team, job, case: caseParam } = useParams<{ team: string; job: string; case: string }>();
  const location = useLocation();
  const caseKey = caseParam ? (caseKeyFromPath(location.pathname) ?? caseParam) : undefined;

  const history = useQuery({
    queryKey: ["testCaseHistory", team, job, caseKey],
    queryFn: () => getTestCaseHistory(team!, job!, caseKey!),
    enabled: !!team && !!job && !!caseKey,
  });

  // The job read builds the trail. It shares the cache entry of the job page,
  // and a failed read keeps the plain job link.
  const jobQuery = useQuery({
    queryKey: ["pipelineJob", team, job],
    queryFn: () => getPipelineJob(team!, job!),
    enabled: !!team && !!job,
  });

  const title = caseTitle(location.state, caseKey ?? "");
  const jobDetail = jobQuery.data;
  useDocumentTitle(
    team && job && caseKey
      ? testCaseTitle(team, jobDetail ? relativeJob(jobDetail.tenantFolder, jobDetail.jobPath) : job, title.name)
      : null,
  );

  if (history.isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.notice}>Loading test case history…</div>
      </div>
    );
  }

  const err = history.error;
  if (err instanceof ApiError && err.status === 403) return <ForbiddenPage />;
  if (err instanceof ApiError && err.status === 404) return <NotFoundPage />;
  if (err instanceof ApiError && err.status === 503) return <RecordStoreDisabledPage />;
  if (err) return <GenericErrorPage onRetry={() => void history.refetch()} />;
  if (!team || !job || !caseKey || !history.data) return <NotFoundPage />;

  const items = history.data.items;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        {jobDetail ? (
          <JobBreadcrumbs
            team={team}
            tenantFolder={jobDetail.tenantFolder}
            jobPath={jobDetail.jobPath}
            current={title.name}
          />
        ) : (
          <Link to={`/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}`} className={styles.jobLink}>
            {job}
          </Link>
        )}
        <h1 className={styles.pageTitle}>
          {title.className && <span className={styles.className}>{title.className}</span>}
          <span className={styles.testName}>{title.name}</span>
        </h1>
        <p className={styles.pageDesc}>
          Recent outcomes of this test case. The page shows only the outcomes inside the record
          store&apos;s retention window. The retention round removes older outcomes.
        </p>
      </div>

      {items.length === 0 ? (
        <div className={styles.empty}>The record store holds no outcome for this test case.</div>
      ) : (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Build</th>
                <th>Status</th>
                <th>Suite</th>
                <th>Duration</th>
                <th>Time</th>
                <th>Error details</th>
              </tr>
            </thead>
            <tbody>
              {items.map((row) => (
                <tr key={`${row.runId}/${row.suite}`}>
                  <td>
                    <Link to={row.runLink}>#{row.buildNumber ?? "?"}</Link>
                  </td>
                  <td>{row.status}</td>
                  <td>{row.suite}</td>
                  <td className={styles.numeric}>
                    {typeof row.durationMs === "number" ? formatDuration(row.durationMs) : null}
                  </td>
                  <td className={styles.numeric} title={new Date(row.createdAt).toLocaleString()}>
                    {age(row.createdAt)}
                  </td>
                  <td>{row.errorDetails ? <pre className={styles.errorDetails}>{row.errorDetails}</pre> : null}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </div>
  );
}
