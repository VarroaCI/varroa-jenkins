import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { listJobBranches } from "../../api/client";
import { Card } from "../../components/Card";
import type { JobBranchHead } from "../../types";
import styles from "./MultibranchCard.module.css";

function enc(value: string): string {
  return encodeURIComponent(value);
}

function jobHref(team: string, jobPath: string): string {
  return `/pipelines/${enc(team)}/${enc(jobPath)}`;
}

function runHref(team: string, jobPath: string, runId: string): string {
  return `${jobHref(team, jobPath)}/${enc(runId)}`;
}

/** shortRevision shortens an SCM revision for a table cell. */
function shortRevision(revision?: string): string {
  return revision ? revision.slice(0, 7) : "";
}

/** RunResult shows the latest run of a head, linked to its build page. */
function RunResult({ team, head }: { team: string; head: JobBranchHead }) {
  if (!head.lastRun) {
    return <span className={styles.dim}>No builds</span>;
  }
  const label = head.lastRun.result ?? head.lastRun.status;
  return (
    <Link to={runHref(team, head.jobPath, head.lastRun.runId)} className={styles.result}>
      {label}
    </Link>
  );
}

/** HeadTable lists one kind of head: branches, tags, or change requests. */
function HeadTable({
  team,
  heads,
  empty,
  showChangeRequest,
}: {
  team: string;
  heads: JobBranchHead[];
  empty: string;
  showChangeRequest: boolean;
}) {
  if (heads.length === 0) {
    return <div className={styles.empty}>{empty}</div>;
  }
  return (
    <table className={styles.table}>
      <thead>
        <tr>
          <th scope="col">{showChangeRequest ? "Pull request" : "Name"}</th>
          <th scope="col">Latest run</th>
          <th scope="col">Revision</th>
        </tr>
      </thead>
      <tbody>
        {heads.map((head) => (
          <tr key={head.jobPath}>
            <td>
              <Link to={jobHref(team, head.jobPath)}>
                {showChangeRequest && head.changeRequest
                  ? `#${head.changeRequest.id} ${head.changeRequest.title ?? ""}`.trim()
                  : head.name}
              </Link>
              {head.orphaned && <span className={styles.tag}>orphaned</span>}
            </td>
            <td>
              <RunResult team={team} head={head} />
            </td>
            <td className={styles.mono}>{shortRevision(head.revision)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/**
 * MultibranchCard lists the branches, tags, and pull requests a
 * multibranch job has discovered. The inventory can be up to ten minutes
 * stale, and the card says when the cell last exported it.
 */
export function MultibranchCard({ team, jobPath }: { team: string; jobPath: string }) {
  const query = useQuery({
    queryKey: ["jobBranches", team, jobPath],
    queryFn: () => listJobBranches(team, jobPath),
    enabled: !!team && !!jobPath,
    // The page holds no run stream for this query, so the latest-run cells
    // refresh on a timer while the card stays open.
    refetchInterval: 30_000,
  });

  if (query.isLoading) {
    return <div className={styles.loading}>Loading branches…</div>;
  }
  if (query.isError || !query.data) {
    return <div className={styles.empty}>The branch inventory could not be loaded.</div>;
  }

  const list = query.data;
  const observed = list.observedAt
    ? new Date(list.observedAt).toLocaleString()
    : null;

  return (
    <div className={styles.stack}>
      <div className={styles.meta}>
        {observed ? (
          <span>Inventory as of {observed}. It can be up to ten minutes old.</span>
        ) : (
          <span>The cell has not exported this job yet.</span>
        )}
        {list.truncated && <span> The cell capped the list.</span>}
      </div>
      <Card title="Branches">
        <HeadTable
          team={team}
          heads={[...list.branches, ...list.tags]}
          empty="No branches discovered."
          showChangeRequest={false}
        />
      </Card>
      <Card title="Pull requests">
        <HeadTable
          team={team}
          heads={list.changeRequests}
          empty="No pull requests discovered."
          showChangeRequest
        />
      </Card>
    </div>
  );
}
