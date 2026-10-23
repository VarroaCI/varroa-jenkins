import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { listJobBranches, scanMultibranch } from "../../api/client";
import { Card } from "../../components/Card";
import type { BranchScan, JobBranchHead, JobBranchList } from "../../types";
import styles from "./MultibranchCard.module.css";

/** How long a requested scan may stay unreported before the card stops waiting. */
const SCAN_WAIT_MS = 10 * 60 * 1000;
/** The refresh interval while a scan is pending. */
const SCAN_POLL_MS = 5_000;
/** The refresh interval of the inventory when no scan is pending. */
const INVENTORY_POLL_MS = 30_000;

function enc(value: string): string {
  return encodeURIComponent(value);
}

function jobHref(tenant: string, jobPath: string): string {
  return `/pipelines/${enc(tenant)}/${enc(jobPath)}`;
}

function runHref(tenant: string, jobPath: string, runId: string): string {
  return `${jobHref(tenant, jobPath)}/${enc(runId)}`;
}

/** shortRevision shortens an SCM revision for a table cell. */
function shortRevision(revision?: string): string {
  return revision ? revision.slice(0, 7) : "";
}

/** RunResult shows the latest run of a head, linked to its build page. */
function RunResult({ tenant, head }: { tenant: string; head: JobBranchHead }) {
  if (!head.lastRun) {
    return <span className={styles.dim}>No builds</span>;
  }
  const label = head.lastRun.result ?? head.lastRun.status;
  return (
    <Link to={runHref(tenant, head.jobPath, head.lastRun.runId)} className={styles.result}>
      {label}
    </Link>
  );
}

/** HeadTable lists one kind of head: branches, tags, or change requests. */
function HeadTable({
  tenant,
  heads,
  empty,
  showChangeRequest,
}: {
  tenant: string;
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
              <Link to={jobHref(tenant, head.jobPath)}>
                {showChangeRequest && head.changeRequest
                  ? `#${head.changeRequest.id} ${head.changeRequest.title ?? ""}`.trim()
                  : head.name}
              </Link>
              {head.orphaned && <span className={styles.tag}>orphaned</span>}
            </td>
            <td>
              <RunResult tenant={tenant} head={head} />
            </td>
            <td className={styles.mono}>{shortRevision(head.revision)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/**
 * RepositoryTable lists the repositories of an organization folder. A
 * repository builds through its own branch jobs, so the table has no run
 * column.
 */
function RepositoryTable({ tenant, repositories }: { tenant: string; repositories: JobBranchHead[] }) {
  return (
    <table className={styles.table}>
      <thead>
        <tr>
          <th scope="col">Repository</th>
        </tr>
      </thead>
      <tbody>
        {repositories.map((repository) => (
          <tr key={repository.jobPath}>
            <td>
              <Link to={jobHref(tenant, repository.jobPath)}>{repository.name}</Link>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** ScanNames lists the head names of one scan result inside a disclosure. */
function ScanNames({ label, names }: { label: string; names: string[] }) {
  if (names.length === 0) {
    return null;
  }
  return (
    <details className={styles.names}>
      <summary>
        {names.length} {label}
      </summary>
      <ul>
        {names.map((name) => (
          <li key={name} className={styles.mono}>
            {name}
          </li>
        ))}
      </ul>
    </details>
  );
}

/** ScanResult states the outcome of one scan: the counts, or the reason it did not finish. */
function ScanResult({ scan }: { scan: BranchScan }) {
  if (scan.state === "running") {
    return <div className={styles.meta}>The scan is running.</div>;
  }
  if (scan.state === "completed") {
    return (
      <div className={styles.scanResult}>
        <div>
          {scan.newHeads.length} new, {scan.changedHeads.length} changed, {scan.orphanedHeads.length} orphaned
        </div>
        <ScanNames label="new" names={scan.newHeads} />
        <ScanNames label="changed" names={scan.changedHeads} />
        <ScanNames label="orphaned" names={scan.orphanedHeads} />
      </div>
    );
  }
  const label =
    scan.state === "timeout"
      ? "The scan timed out."
      : scan.state === "refused"
        ? "The scan was refused."
        : "The scan failed.";
  return (
    <div className={styles.scanResult}>
      {label} {scan.message ?? ""}
    </div>
  );
}

/** scanPending reports whether a requested scan has not reported yet. */
function scanPending(lastScan: BranchScan | undefined, scanId: string, startedAt: number, now: number): boolean {
  if (now - startedAt >= SCAN_WAIT_MS) {
    return false;
  }
  return !lastScan || lastScan.scanId !== scanId || lastScan.state === "running";
}

/**
 * MultibranchCard lists the branches, tags, and pull requests a
 * multibranch job or an organization folder has discovered. The inventory can be up to ten minutes
 * stale, and the card says when the cell last exported it. A caller who
 * may write the job sees a Scan now button. After a scan starts, the card
 * refreshes the inventory every few seconds until the scan reports.
 */
export function MultibranchCard({
  tenant,
  jobPath,
  canScan = false,
}: {
  tenant: string;
  jobPath: string;
  canScan?: boolean;
}) {
  const queryClient = useQueryClient();
  const [scan, setScan] = useState<{ scanId: string; startedAt: number } | null>(null);
  const [scanError, setScanError] = useState<string | null>(null);

  const query = useQuery({
    queryKey: ["jobBranches", tenant, jobPath],
    queryFn: () => listJobBranches(tenant, jobPath),
    enabled: !!tenant && !!jobPath,
    // The page holds no run stream for this query, so the latest-run cells
    // refresh on a timer while the card stays open. A pending scan shortens
    // the timer until its result arrives.
    refetchInterval: (q) => {
      const data = q.state.data as JobBranchList | undefined;
      if (scan && scanPending(data?.lastScan, scan.scanId, scan.startedAt, Date.now())) {
        return SCAN_POLL_MS;
      }
      return INVENTORY_POLL_MS;
    },
  });

  const start = useMutation({
    mutationFn: () => scanMultibranch(tenant, jobPath),
    onMutate: () => setScanError(null),
    onSuccess: (accepted) => {
      setScan({ scanId: accepted.scanId, startedAt: Date.now() });
      void queryClient.invalidateQueries({ queryKey: ["jobBranches", tenant, jobPath] });
    },
    onError: () => setScanError("The scan could not be requested."),
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
  // The result shows the scan this page started, or the last scan the cell
  // reported when this page started none.
  const shownScan =
    list.lastScan && (!scan || list.lastScan.scanId === scan.scanId) ? list.lastScan : undefined;
  const waiting =
    scan !== null &&
    scanPending(list.lastScan, scan.scanId, scan.startedAt, Date.now());

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
      {canScan && (
        <div className={styles.scanBar}>
          <button
            type="button"
            className={styles.scanButton}
            onClick={() => start.mutate()}
            disabled={start.isPending || waiting}
          >
            Scan now
          </button>
          {waiting && <span className={styles.meta}>Waiting for the scan to report…</span>}
          {scanError && <span className={styles.error}>{scanError}</span>}
        </div>
      )}
      {shownScan && (
        <Card title="Last scan">
          <ScanResult scan={shownScan} />
        </Card>
      )}
      {scan && !shownScan && !waiting && (
        <Card title="Last scan">
          <div className={styles.meta}>The scan did not report within ten minutes. Refresh the page later.</div>
        </Card>
      )}
      {list.repositories.length > 0 && (
        <Card title="Repositories">
          <RepositoryTable tenant={tenant} repositories={list.repositories} />
        </Card>
      )}
      <Card title="Branches">
        <HeadTable
          tenant={tenant}
          heads={[...list.branches, ...list.tags]}
          empty="No branches discovered."
          showChangeRequest={false}
        />
      </Card>
      <Card title="Pull requests">
        <HeadTable
          tenant={tenant}
          heads={list.changeRequests}
          empty="No pull requests discovered."
          showChangeRequest
        />
      </Card>
    </div>
  );
}
