import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { age } from "../components/activityTimeline.util";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../components/RecoveryState";
import { listAllFleetLocks, releaseFleetLock } from "../api/client";
import { useAuth } from "../context/AuthContext";
import { ApiError } from "../hooks/useApi";
import { canDoGlobal } from "../hooks/usePermissions";
import { holderOf, holdsLock } from "../lib/fleetLocks";
import { controllerRoute } from "../routing";
import type { FleetLock, FleetLockHolderCell } from "../types";
import styles from "./Locks.module.css";

/** CELLS_POLL_MS is the cadence the cells page reads at. This page holds
 * the same kind of fleet-wide list, so it polls at that cadence and
 * opens no stream. */
const CELLS_POLL_MS = 15_000;

/** LOCKS_KEY is the query key of the fleet-wide lock list. */
const LOCKS_KEY = ["fleet-locks", "all"];

/**
 * LockRow renders one fleet lock. A held or a releasing row names its own
 * run. A waiting row names the row of the same lock name that holds the
 * lock, and the page groups that row from the list it already holds. When
 * this list carries no holder row, the row states that the lock is held
 * elsewhere. The row shows no queue position, because no read answers one.
 */
function LockRow({
  items,
  row,
  canRelease,
  onRelease,
}: {
  items: FleetLock[];
  row: FleetLock;
  canRelease: boolean;
  onRelease: (row: FleetLock) => void;
}) {
  const waiting = row.state === "waiting";
  const holder = waiting ? holderOf(items, row) : holdsLock(row) ? row : undefined;
  const since = waiting ? row.waitingSince : row.acquiredAt;
  return (
    <tr>
      <td className={styles.nameCell}>{row.name}</td>
      <td>{row.cluster}</td>
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
      {canRelease && (
        <td>
          {row.state === "releasing" && (
            <Button size="sm" onClick={() => onRelease(row)}>
              Force release
            </Button>
          )}
        </td>
      )}
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

/**
 * releaseRefusal states why a release failed. It names the HTTP status
 * and the server's own message, because the server message names the
 * rule: a lock that is not releasing, a holder pod that still runs, or a
 * lock that is gone.
 */
function releaseRefusal(err: unknown): string {
  if (err instanceof ApiError) {
    const text = err.serverMessage || err.message;
    return err.status > 0 ? `${err.status}: ${text}` : text;
  }
  return err instanceof Error && err.message ? err.message : "The release failed.";
}

/**
 * holderCellOf answers the holder cell a 409 refusal names, or null. Only a
 * refusal for a live holder pod carries one.
 */
function holderCellOf(err: unknown): FleetLockHolderCell | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const body = err.body;
  if (!body || typeof body !== "object" || !("holderCell" in body)) return null;
  const cell = (body as { holderCell?: FleetLockHolderCell }).holderCell;
  return cell && cell.cluster && cell.namespace && cell.name ? cell : null;
}

/**
 * ReleaseDialog asks for the reason of one force release. The reason is
 * required, because the audit record carries it. The page reads the list
 * again after every attempt, so a release that landed with no reply also
 * shows.
 */
function ReleaseDialog({ row, onClose }: { row: FleetLock; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [reason, setReason] = useState("");
  const release = useMutation({
    mutationFn: () => releaseFleetLock({ name: row.name, runId: row.runId, reason: reason.trim() }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: LOCKS_KEY });
      onClose();
    },
    // A refusal reads the list too: an unconfirmed release can have landed.
    onError: () => {
      void queryClient.invalidateQueries({ queryKey: LOCKS_KEY });
    },
  });
  const blank = reason.trim() === "";
  const holderCell = release.isError ? holderCellOf(release.error) : null;

  // Escape closes the dialog, as every other dialog of the app does.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className={styles.overlay}
      onMouseDown={(e) => {
        // Only a press that starts on the backdrop closes the dialog. A
        // drag that ends there keeps the typed reason.
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        className={styles.dialog}
        role="dialog"
        aria-modal="true"
        aria-labelledby="force-release-title"
      >
        <h2 id="force-release-title" className={styles.dialogTitle}>
          Force release {row.name}
        </h2>
        <p className={styles.dialogText}>
          Run {row.runId} of {row.tenant} on {row.cluster}. Release the lock only when the
          holder&apos;s build runs on no cell.
        </p>
        <label className={styles.field}>
          <span>Reason</span>
          <textarea
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={3}
            aria-label="Reason"
            required
            autoFocus
          />
        </label>
        {release.isError && (
          <p className={styles.refusal} role="alert">
            {releaseRefusal(release.error)}
          </p>
        )}
        {holderCell && (
          <p className={styles.dialogText}>
            Restart cell{" "}
            <Link to={controllerRoute(holderCell.cluster, holderCell.namespace, holderCell.name)}>
              {holderCell.namespace}/{holderCell.name}
            </Link>{" "}
            on {holderCell.cluster} when its Jenkins does not run. The lock then releases.
          </p>
        )}
        <div className={styles.dialogActions}>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={() => release.mutate()}
            disabled={blank || release.isPending}
          >
            {release.isPending ? "Releasing…" : "Force release"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export default function Locks() {
  const { permissions } = useAuth();
  const canRelease = canDoGlobal(permissions, "fleetlocks", "release");
  const [releasing, setReleasing] = useState<FleetLock | null>(null);
  const locks = useQuery({
    queryKey: LOCKS_KEY,
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
            Every tenant's fleet locks across every cluster. A lock leaves when its run releases
            it, or when an operator force-releases a releasing lock.
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
                <th>Cluster</th>
                <th>Tenant</th>
                <th>Job</th>
                <th>State</th>
                <th>Holder</th>
                <th>Since</th>
                {canRelease && <th aria-label="Actions" />}
              </tr>
            </thead>
            <tbody>
              {items.map((row) => (
                <LockRow
                  key={`${row.name}/${row.runId}`}
                  items={items}
                  row={row}
                  canRelease={canRelease}
                  onRelease={setReleasing}
                />
              ))}
            </tbody>
          </table>
        </Card>
      )}

      {releasing && <ReleaseDialog row={releasing} onClose={() => setReleasing(null)} />}
    </div>
  );
}
