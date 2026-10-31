import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import { StatusPill } from "../components/StatusPill";
import { age } from "../components/activityTimeline.util";
import { cellUsageKey, useCellPools, useCellUsageIndex } from "../hooks/useCells";
import { useControllers } from "../hooks/useControllers";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import { patchCellPool } from "../api/client";
import { ApiError } from "../hooks/useApi";
import { controllerRoute } from "../routing";
import type { ControllerListItem } from "../hooks/useControllers";
import type { CellPool, CellPoolListResponse, CellPoolScaleRequest, CellUsage } from "../types";
import type { CellUsageIndex } from "../hooks/useCells";
import styles from "./Cells.module.css";

/** An absent value renders as an em dash, never as zero. */
const DASH = "—";

export default function Cells() {
  const { permissions } = useAuth();
  const canScale = canDoGlobal(permissions, "cellpools", "update");

  const pools = useCellPools();
  const poolItems = pools.data?.items ?? [];
  // The list sorts the BFF's own cluster first. A controllers row with no
  // cluster belongs to that cluster.
  const localCluster = pools.data?.clusters?.[0]?.name ?? "";
  const unreachable = (pools.data?.clusters ?? []).filter((c) => !c.ok);
  const controllers = useControllers();
  const cells = (controllers.data ?? []).filter((c) => c.kind === "cell");

  // The batch usage read covers one cluster, so the page reads it once per
  // cluster the list names. A cell name is unique inside one cluster only.
  const clusters = useMemo(
    () => Array.from(new Set(cells.map((c) => c.cluster))).sort(),
    // The identity of the cells array changes on every poll, so the memo
    // keys on the cluster names themselves.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [cells.map((c) => c.cluster).sort().join(",")],
  );
  const usage = useCellUsageIndex(clusters);

  // Every cluster answered 503, so no usage column has a source. The table
  // drops the four columns and the page states why. A partial answer keeps
  // the columns and marks the uncovered rows instead.
  const usageDisabled =
    clusters.length > 0 &&
    usage.covered.size === 0 &&
    usage.recordStoreDisabled.size === clusters.length;
  // A store that did not answer keeps the four columns. Each row reads not
  // available, and the notice offers Retry.
  const usageUnreachable = usage.recordStoreUnreachable.size > 0;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <h1 className={styles.pageTitle}>Cells</h1>
          <p className={styles.pageDesc}>
            One block for each cell pool of each cluster, with the cells that pool owns. The table
            lists the cells you may read.
          </p>
        </div>
      </div>

      {usageDisabled && (
        <div className={styles.notice}>
          The build record store is disabled, so this page states no tenant fill, queue, run, or
          failure count. Enable <code>recordStore</code> to see them.
        </div>
      )}
      {usageUnreachable && (
        <div className={styles.notice}>
          The build record store did not answer for one or more clusters. Their rows show no tenant
          fill, queue, run, or failure count. The page tries the read again every 15 seconds.{" "}
          <Button size="sm" variant="ghost" onClick={usage.refetch}>
            Retry
          </Button>
        </div>
      )}
      {pools.error && !(pools.error instanceof ApiError && pools.error.status === 503) && (
        <div className={styles.notice}>Failed to read the cell pools: {pools.error.message}</div>
      )}

      {unreachable.map((c) => (
        <div key={c.name} className={styles.notice}>
          The pools of cluster <code>{c.name}</code> are absent: {c.error || "the cluster did not answer"}.
        </div>
      ))}

      {pools.isLoading && <div className={styles.notice}>Reading the cell pools…</div>}
      {!pools.isLoading && poolItems.length === 0 && unreachable.length === 0 && (
        <div className={styles.notice}>No cell pool exists yet.</div>
      )}

      {poolItems.map((pool) => (
        <PoolBlock
          key={`${pool.cluster}/${pool.name}`}
          pool={pool}
          cells={cells.filter(
            (c) => (c.cluster || localCluster) === pool.cluster && c.cellPool === pool.name,
          )}
          usage={usage}
          usageDisabled={usageDisabled}
          controllersLoading={controllers.isLoading}
          usageLoading={usage.isLoading}
          canScale={canScale}
        />
      ))}
    </div>
  );
}

function PoolBlock({
  pool,
  cells,
  usage,
  usageDisabled,
  controllersLoading,
  usageLoading,
  canScale,
}: {
  pool: CellPool;
  cells: ControllerListItem[];
  usage: CellUsageIndex;
  usageDisabled: boolean;
  controllersLoading: boolean;
  usageLoading: boolean;
  canScale: boolean;
}) {
  const ready = pool.status.conditions?.find((c) => c.type === "PoolReady");
  // Any PoolReady status other than True, Unknown included, is not ready.
  const notReady = ready != null && ready.status !== "True";
  const poolCap = pool.spec.maxTenantsPerCell;
  const differing = cells
    .map((c) => usage.rows.get(cellUsageKey(c.cluster, c.namespace, c.name)))
    .find((u) => u != null && poolCap != null && u.maxTenants !== poolCap);
  const readyText = ready ? `${ready.status}${ready.reason ? ` (${ready.reason})` : ""}` : DASH;

  return (
    <Card className={notReady ? styles.poolNotReady : undefined}>
      <div className={styles.poolHead}>
        <div>
          <h2 className={styles.poolName}>
            <span className={styles.poolCluster}>{pool.cluster} / </span>
            {pool.name}
          </h2>
          <p className={styles.poolProfile}>Profile {pool.spec.profileRef}</p>
        </div>
        {canScale && <ScaleControl pool={pool} />}
      </div>

      {notReady && (
        <div className={styles.notReady} role="status">
          Pool not ready: {ready.reason || ready.status}.{ready.message ? ` ${ready.message}` : ""}
        </div>
      )}

      <dl className={styles.counts}>
        <Count label="Warm cells" value={pool.status.warmCells} />
        <Count label="Placed cells" value={pool.status.placedCells} />
        <Count label="Unready cells" value={pool.status.unreadyCells} />
        <Count label="Free slots on placed cells" value={pool.status.freeTenantSlots} />
        <Count label="Max cells" value={pool.spec.maxCells ? pool.spec.maxCells : "No limit"} />
        <Count
          label="Pool ready"
          value={<span className={notReady ? styles.warnValue : undefined}>{readyText}</span>}
        />
      </dl>

      {differing && poolCap != null && (
        <p className={styles.note}>
          This pool sets a tenant cap of {poolCap}. A cell below reports a different cap, because
          its own <code>spec.cell.maxTenants</code> wins. Placement resolves capacity in that
          order, never as the smaller of two values.
        </p>
      )}

      <div className={styles.rule} />

      {cells.length === 0 ? (
        <p className={styles.empty}>
          {pool.spec.warm === 0 || pool.spec.warm == null
            ? "This pool owns no cell: warm is zero and no tenant waits for one."
            : "This pool owns no cell: no Tenant requests its profile yet."}
        </p>
      ) : (
        <div className={styles.tableScroll}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Cell</th>
                <th>Phase</th>
                {!usageDisabled && <th>Tenants / cap</th>}
                {!usageDisabled && <th>Queue</th>}
                {!usageDisabled && <th>Runs 24 h</th>}
                {!usageDisabled && <th>Failures 24 h</th>}
                <th>Age</th>
                <th>Unready since</th>
              </tr>
            </thead>
            <tbody>
              {controllersLoading || usageLoading
                ? cells.map((c) => <SkeletonRow key={`${c.namespace}/${c.name}`} usageDisabled={usageDisabled} />)
                : cells.map((c) => (
                    <CellRow
                      key={cellUsageKey(c.cluster, c.namespace, c.name)}
                      cell={c}
                      usage={usage.rows.get(cellUsageKey(c.cluster, c.namespace, c.name))}
                      covered={usage.covered.has(c.cluster)}
                      usageDisabled={usageDisabled}
                    />
                  ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function Count({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className={styles.count}>
      <dt>{label}</dt>
      <dd>{value}</dd>
    </div>
  );
}

function SkeletonRow({ usageDisabled }: { usageDisabled: boolean }) {
  const columns = usageDisabled ? 4 : 8;
  return (
    <tr>
      {Array.from({ length: columns }).map((_, i) => (
        <td key={i}>
          <span className={styles.skeleton} />
        </td>
      ))}
    </tr>
  );
}

function CellRow({
  cell,
  usage,
  covered,
  usageDisabled,
}: {
  cell: ControllerListItem;
  usage?: CellUsage;
  /** True when the usage read covered this cell's own cluster. */
  covered: boolean;
  usageDisabled: boolean;
}) {
  // An uncovered cluster has no usage data at all. That state is not an
  // absent gauge and not a count of zero, so it reads differently.
  const notAvailable = <span className={styles.muted}>not available</span>;
  return (
    <tr>
      <td>
        <Link to={`${controllerRoute(cell.cluster, cell.namespace, cell.name)}?tab=cell`}>
          {cell.name}
        </Link>
        <div className={styles.sub}>ns/{cell.namespace}</div>
      </td>
      <td>
        <StatusPill phase={cell.phase} attention={cell.attention} />
      </td>
      {!usageDisabled && (
        <td>
          {!covered ? notAvailable : usage ? `${usage.tenants.length} / ${usage.maxTenants}` : DASH}
        </td>
      )}
      {!usageDisabled && (
        <td>
          {!covered ? (
            notAvailable
          ) : usage?.queueLength != null ? (
            <span title={usage.queueLengthAt ? `Read at ${usage.queueLengthAt}` : undefined}>
              {usage.queueLength}
            </span>
          ) : (
            <span className={styles.muted} title="The mite gauges cover the local cluster only.">
              {DASH}
            </span>
          )}
        </td>
      )}
      {!usageDisabled && <td>{!covered ? notAvailable : usage ? usage.totals.runsLast24h : DASH}</td>}
      {!usageDisabled && <td>{!covered ? notAvailable : usage ? usage.totals.failedLast24h : DASH}</td>}
      <td>{cell.createdAt ? age(cell.createdAt) : DASH}</td>
      <td>{cell.unreadySince ? age(cell.unreadySince) : DASH}</td>
    </tr>
  );
}

/** The two dials the scale control edits. */
type ScaleDial = "warm" | "maxCells";

function ScaleControl({ pool }: { pool: CellPool }) {
  const queryClient = useQueryClient();
  const storedWarm = pool.spec.warm ?? 0;
  const storedMaxCells = pool.spec.maxCells ?? 0;
  // Zero means no limit, so the max-cells field shows an empty input for it.
  const seedMaxCells = (n: number) => (n === 0 ? "" : String(n));
  const [warm, setWarm] = useState(String(storedWarm));
  const [maxCells, setMaxCells] = useState(seedMaxCells(storedMaxCells));
  // dirty names the dials this caller typed into. A dial the caller never
  // touched is not an edit, whatever the poll later reports: comparing the
  // input against the live spec alone would turn another writer's change
  // into a pending edit and write it back.
  const [dirty, setDirty] = useState<Set<ScaleDial>>(new Set());
  const [confirming, setConfirming] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  // An untouched input re-seeds from the live spec, so the control shows
  // what the pool stores while the caller's own typing survives the poll.
  useEffect(() => {
    if (!dirty.has("warm")) setWarm(String(storedWarm));
  }, [storedWarm, dirty]);
  useEffect(() => {
    if (!dirty.has("maxCells")) setMaxCells(seedMaxCells(storedMaxCells));
  }, [storedMaxCells, dirty]);

  const edit = (dial: ScaleDial, value: string) => {
    setDirty((prev) => new Set(prev).add(dial));
    if (dial === "warm") setWarm(value);
    else setMaxCells(value);
  };

  // The PATCH takes any subset of the dials. Sending an unchanged value
  // would revert a concurrent edit by another operator, so the body carries
  // only the dials this caller typed into and then changed.
  const changed: CellPoolScaleRequest["spec"] = {};
  if (dirty.has("warm") && Number(warm) !== storedWarm) changed.warm = Number(warm);
  if (dirty.has("maxCells") && Number(maxCells) !== storedMaxCells) {
    changed.maxCells = Number(maxCells);
  }
  const hasChange = Object.keys(changed).length > 0;

  const scale = useMutation({
    mutationFn: () => patchCellPool(pool.name, pool.cluster, { spec: changed }),
    onSuccess: (updated) => {
      setConfirming(false);
      setConflict(false);
      setRefusal(null);
      // The write landed, so the inputs follow the stored pool again.
      setDirty(new Set());
      // The response carries the stored pool, so the block renders the
      // written values without waiting for the next poll.
      // A pool name is unique inside one cluster only, so the merge
      // matches both the cluster and the name.
      queryClient.setQueriesData<CellPoolListResponse>({ queryKey: ["cellpools"] }, (current) =>
        current
          ? {
              ...current,
              items: current.items.map((p) =>
                p.cluster === updated.cluster && p.name === updated.name ? updated : p,
              ),
            }
          : current,
      );
      void queryClient.invalidateQueries({ queryKey: ["cellpools"] });
    },
    onError: (err) => {
      setConfirming(false);
      // A conflict keeps the typed values. The control never retries on its
      // own.
      if (err instanceof ApiError && err.status === 409) {
        setConflict(true);
        void queryClient.invalidateQueries({ queryKey: ["cellpools"] });
        return;
      }
      // Every other refusal states the server's own message. A 400 names
      // the dial rule the write broke, and the fixed status copy would hide
      // it, so the control leaves the caller with no way to correct it.
      if (err instanceof ApiError && err.serverMessage) {
        setRefusal(err.serverMessage);
        return;
      }
      setRefusal(err instanceof Error && err.message ? err.message : "The write was refused.");
    },
  });

  // A drain is the only case that confirms. The confirm names the reported
  // warm-cell count and the typed value, and it computes no difference. An
  // edit that leaves warm alone drains nothing, whatever the pool reports.
  const drains = changed.warm !== undefined && changed.warm < pool.status.warmCells;

  const apply = () => {
    setConflict(false);
    setRefusal(null);
    if (!hasChange) return;
    if (drains && !confirming) {
      setConfirming(true);
      return;
    }
    scale.mutate();
  };

  return (
    <div className={styles.scale}>
      <label className={styles.field}>
        <span>Warm</span>
        <input
          type="number"
          min={0}
          value={warm}
          onChange={(e) => edit("warm", e.target.value)}
          aria-label={`Warm cells of ${pool.name} on ${pool.cluster}`}
        />
      </label>
      <label className={styles.field}>
        <span>Max cells</span>
        <small className={styles.hint}>Empty or 0: no limit</small>
        <input
          type="number"
          min={0}
          value={maxCells}
          placeholder="No limit"
          onChange={(e) => edit("maxCells", e.target.value)}
          aria-label={`Max cells of ${pool.name} on ${pool.cluster}`}
        />
      </label>
      <Button size="sm" onClick={apply} disabled={scale.isPending}>
        {scale.isPending ? "Applying…" : "Apply"}
      </Button>
      {confirming && (
        <div className={styles.confirm} role="alert">
          <p>
            This pool reports {pool.status.warmCells} warm cells. You typed {warm}. The drain
            removes free cells only. A cell that holds a tenant never drains.
          </p>
          <div className={styles.confirmActions}>
            <Button size="sm" onClick={() => scale.mutate()} disabled={scale.isPending}>
              Yes, drain
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
          </div>
        </div>
      )}
      {conflict && (
        <p className={styles.conflict} role="alert">
          Another writer changed this pool. The pool is read again and your typed values are kept.
          Press Apply to write them.
        </p>
      )}
      {refusal && (
        <p className={styles.conflict} role="alert">
          {refusal}
        </p>
      )}
    </div>
  );
}
