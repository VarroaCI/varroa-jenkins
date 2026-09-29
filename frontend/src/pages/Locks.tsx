import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Card } from "../components/Card";
import { age } from "../components/activityTimeline.util";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../components/RecoveryState";
import { listAllFleetLocks } from "../api/client";
import { ApiError } from "../hooks/useApi";
import { holderOf, holdsLock } from "../lib/fleetLocks";
import type { FleetLock } from "../types";
import styles from "./Locks.module.css";

/** CELLS_POLL_MS is the cadence the cells page reads at. This page holds
 * the same kind of cluster-wide list, so it polls at that cadence and
 * opens no stream. */
const CELLS_POLL_MS = 15_000;

/**
 * LockRow renders one fleet lock. A held or a releasing row names its own
 * run. A waiting row names the row of the same lock name that holds the
 * lock, and the page groups that row from the list it already holds. When
 * this list carries no holder row, the row states that the lock is held
 * elsewhere. The row shows no queue position, because no read answers one.
 */
function LockRow({ items, row }: { items: FleetLock[]; row: FleetLock }) {
  const waiting = row.state === "waiting";
  const holder = waiting ? holderOf(items, row) : holdsLock(row) ? row : undefined;
  const since = waiting ? row.waitingSince : row.acquiredAt;
  return (
    <tr>
      <td className={styles.nameCell}>{row.name}</td>
      <td>{row.tenant}</td>
      <td className={styles.jobCell}>{row.jobPath}</td>
      <td>{row.state}</td>
      <td>
        {holder ? (
          <HolderRun row={holder} />
        ) : waiting ? (
          <span className={styles.muted}>held elsewhere</span>
        ) : (
          <span className={styles.muted}>—</span>
        )}
      </td>
      <td className={styles.lockSince}>
        {since ? age(since) : <span className={styles.muted}>—</span>}
      </td>
    </tr>
  );
}

/**
 * HolderRun names the run that holds a lock. The cell links to the run
 * when the response carries a runLink. A lock the caller cannot read
 * states its job path as plain text.
 */
function HolderRun({ row }: { row: FleetLock }) {
  if (!row.runLink) return <span className={styles.jobCell}>{row.jobPath}</span>;
  return (
    <Link to={row.runLink} className={styles.jobCell}>
      {row.jobPath}
    </Link>
  );
}

export default function Locks() {
  const locks = useQuery({
    queryKey: ["fleet-locks", "all"],
    queryFn: listAllFleetLocks,
    refetchInterval: CELLS_POLL_MS,
  });

  const items = useMemo(() => locks.data?.items ?? [], [locks.data]);

  if (locks.isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.notice}>Reading the fleet locks…</div>
      </div>
    );
  }

  if (locks.error instanceof ApiError && locks.error.status === 403) return <ForbiddenPage />;
  if (locks.error instanceof ApiError && locks.error.status === 404) return <NotFoundPage />;
  if (locks.error instanceof ApiError && locks.error.status === 503) return <RecordStoreDisabledPage />;
  if (locks.error) return <GenericErrorPage />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <h1 className={styles.pageTitle}>Fleet locks</h1>
          <p className={styles.pageDesc}>
            Every tenant's fleet locks in this cluster. This view reads only; a lock leaves when
            the run that holds it releases the lock.
          </p>
        </div>
      </div>

      {items.length === 0 ? (
        <div className={styles.empty}>No tenant holds a fleet lock.</div>
      ) : (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Lock</th>
                <th>Tenant</th>
                <th>Job</th>
                <th>State</th>
                <th>Holder</th>
                <th>Since</th>
              </tr>
            </thead>
            <tbody>
              {items.map((row) => (
                <LockRow key={`${row.name}/${row.runId}`} items={items} row={row} />
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </div>
  );
}
