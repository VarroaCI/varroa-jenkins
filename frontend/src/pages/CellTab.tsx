import { Link } from "react-router-dom";
import { Card } from "../components/Card";
import { KVGrid } from "../components/KVGrid";
import { age } from "../components/activityTimeline.util";
import { useCellGrants, useCellPool, useCellUsage } from "../hooks/useCells";
import { useAuth } from "../context/AuthContext";
import { canAccessTenantPipeline, canDoGlobal } from "../hooks/usePermissions";
import { ApiError } from "../hooks/useApi";
import type { ControllerDetail } from "../hooks/useControllers";
import type { CellPool, CellTenantGrants, CellUsage } from "../types";
import styles from "./CellTab.module.css";

/** An absent value renders as an em dash, never as zero. */
const DASH = "—";

interface CellTabProps {
  ctrl: ControllerDetail;
  cluster: string;
  namespace: string;
  name: string;
  /** True while the Cell tab is the active tab. The reads run only then. */
  active: boolean;
}

export function CellTab({ ctrl, cluster, namespace, name, active }: CellTabProps) {
  const { permissions } = useAuth();
  const canReadPools = canDoGlobal(permissions, "cellpools", "read");

  const usage = useCellUsage(namespace, name, active, cluster);
  const poolName = usage.data?.cellPool;
  const pool = useCellPool(poolName, active && canReadPools, cluster);
  const grants = useCellGrants(namespace, name, active, cluster);

  const cell = ctrl.cell;
  const tenants = cell?.tenants ?? [];
  const applied = new Set(cell?.appliedTenants ?? []);

  return (
    <div className={styles.tab}>
      <TopologyPanel
        cell={cell}
        usage={usage.data}
        pool={pool.data}
        canReadPools={canReadPools}
        tenants={tenants}
        applied={applied}
        permissions={permissions}
      />
      <CapacityPanel usage={usage.data} pool={pool.data} canReadPools={canReadPools} />
      <PermissionsPanel
        tenants={tenants}
        entries={grants.data?.tenants}
        error={grants.error}
        loading={grants.isLoading}
      />
    </div>
  );
}

type CellProjection = ControllerDetail["cell"];
type Permissions = ReturnType<typeof useAuth>["permissions"];

function TopologyPanel({
  cell,
  usage,
  pool,
  canReadPools,
  tenants,
  applied,
  permissions,
}: {
  cell: CellProjection;
  usage?: CellUsage;
  pool?: CellPool;
  canReadPools: boolean;
  tenants: string[];
  applied: Set<string>;
  permissions: Permissions;
}) {
  const items = [
    { key: "Pool", value: usage?.cellPool || DASH },
    { key: "Profile", value: usage?.profile || DASH },
    { key: "Cell instance", value: <span className={styles.mono}>{cell?.instance || DASH}</span> },
    { key: "Previous instance", value: <span className={styles.mono}>{cell?.previousInstance || DASH}</span> },
    {
      key: "Evacuate",
      value: cell?.evacuate ? (
        <span className={styles.flag}>Set by the operator</span>
      ) : (
        "Not set"
      ),
    },
    { key: "Unready since", value: cell?.unreadySince ? age(cell.unreadySince) : DASH },
  ];
  if (canReadPools) {
    items.push({ key: "Pool tenant cap", value: String(pool?.spec.maxTenantsPerCell ?? DASH) });
    items.push({ key: "Pool tenant run cap", value: pool?.spec.maxRunsPerTenant ? String(pool.spec.maxRunsPerTenant) : "None" });
    items.push({ key: "Pool ready", value: poolReadyLine(pool) });
    items.push({ key: "Pool approved signatures", value: poolSignaturesLine(pool) });
  }

  // The role, the agent namespace, and the placement generation come from
  // the cell usage read, which reads the record store. The record store
  // holds a member cluster's tenants too, so a member cell shows them the
  // same way. The tab computes nothing: a standby holds the tenant folder
  // and runs no build.
  const usageByTenant = new Map((usage?.tenants ?? []).map((row) => [row.tenant, row]));

  return (
    <Card>
      <h3 className={styles.panelTitle}>Topology</h3>
      <KVGrid items={items} />
      {!canReadPools && (
        <p className={styles.note}>
          The pool tenant cap and the pool verdict need the cellpools read grant.
        </p>
      )}
      {cell?.tenantWarnings && cell.tenantWarnings.length > 0 && (
        <ul className={styles.warnList}>
          {cell.tenantWarnings.map((w, i) => (
            <li key={i} className={styles.warnItem}>
              <b>{w.tenant}</b> {w.reason}: {w.message}
            </li>
          ))}
        </ul>
      )}

      <h4 className={styles.subTitle}>Tenants</h4>
      {tenants.length === 0 ? (
        <p className={styles.empty}>This cell holds no tenant. It is a free cell of its pool.</p>
      ) : (
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Tenant</th>
              <th>Folder</th>
              <th>Role</th>
              <th>Placement</th>
              <th>Agent namespace</th>
              <th>Placement generation</th>
            </tr>
          </thead>
          <tbody>
            {tenants.map((tenant) => {
              const row = usageByTenant.get(tenant);
              const canOpen = canAccessTenantPipeline(permissions, tenant, "read");
              return (
                <tr key={tenant}>
                  <td>
                    {canOpen ? (
                      <Link to={`/pipelines/${encodeURIComponent(tenant)}`}>{tenant}</Link>
                    ) : (
                      tenant
                    )}
                  </td>
                  <td className={styles.mono}>{tenant}</td>
                  <td>{row?.role || DASH}</td>
                  <td>{applied.has(tenant) ? "placed" : "rehydrating"}</td>
                  <td className={styles.mono}>{row?.agentNamespace || DASH}</td>
                  <td>{row?.placementGeneration ?? DASH}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </Card>
  );
}

function poolReadyLine(pool?: CellPool): string {
  const condition = pool?.status.conditions?.find((c) => c.type === "PoolReady");
  if (!condition) return DASH;
  const reason = condition.reason ? ` (${condition.reason})` : "";
  return `${condition.status}${reason}`;
}

// poolSignaturesLine counts the signatures the pool approves. It names none
// of them: the list is long, and the pool declaration holds the detail.
function poolSignaturesLine(pool?: CellPool): string {
  if (!pool) return DASH;
  const count = pool.spec.approvedSignatures?.length ?? 0;
  return count === 0 ? "None" : String(count);
}

function CapacityPanel({
  usage,
  pool,
  canReadPools,
}: {
  usage?: CellUsage;
  pool?: CellPool;
  canReadPools: boolean;
}) {
  const poolCap = pool?.spec.maxTenantsPerCell;
  const capsDiffer = canReadPools && poolCap != null && usage != null && poolCap !== usage.maxTenants;

  return (
    <Card>
      <h3 className={styles.panelTitle}>Capacity and usage</h3>
      <KVGrid
        items={[
          { key: "Tenant cap", value: usage ? String(usage.maxTenants) : DASH },
          { key: "Free slots", value: usage ? String(usage.freeSlots) : DASH },
          {
            key: "Queue length",
            value:
              usage?.queueLength != null ? (
                <span title={usage.queueLengthAt ? `Read at ${usage.queueLengthAt}` : undefined}>
                  {usage.queueLength}
                </span>
              ) : (
                <span className={styles.muted}>
                  {DASH} <small>no gauge: the mite gauges cover the local cluster only</small>
                </span>
              ),
          },
        ]}
      />
      {capsDiffer && (
        <p className={styles.note}>
          The owning pool reports a tenant cap of {poolCap}. Placement resolves capacity in order:
          the cell&apos;s own value, then the pool value, then the chart value. It is never the
          smaller of two values.
        </p>
      )}

      <h4 className={styles.subTitle}>Per-tenant counts</h4>
      <p className={styles.note}>
        These counts come from the build records. The header reports the Jenkins instance, so the
        two numbers can differ.
        {usage?.windowStart ? ` The 24-hour window starts at ${usage.windowStart}.` : ""}
      </p>
      {!usage || usage.tenants.length === 0 ? (
        <p className={styles.empty}>No tenant has run a build in the window.</p>
      ) : (
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Tenant</th>
              <th>Running now</th>
              <th>Runs 24 h</th>
              <th>Failures 24 h</th>
              <th>Jobs</th>
            </tr>
          </thead>
          <tbody>
            {usage.tenants.map((t) => (
              <tr key={t.tenant}>
                <td>{t.tenant}</td>
                <td>{t.runningNow}</td>
                <td>{t.runsLast24h}</td>
                <td>{t.failedLast24h}</td>
                <td>{t.jobs}</td>
              </tr>
            ))}
            <tr className={styles.totalRow}>
              <td>Total</td>
              <td>{usage.totals.runningNow}</td>
              <td>{usage.totals.runsLast24h}</td>
              <td>{usage.totals.failedLast24h}</td>
              <td>{usage.totals.jobs}</td>
            </tr>
          </tbody>
        </table>
      )}
    </Card>
  );
}

function PermissionsPanel({
  tenants,
  entries,
  error,
  loading,
}: {
  tenants: string[];
  entries?: CellTenantGrants[];
  error: unknown;
  loading: boolean;
}) {
  const refused = error instanceof ApiError && error.status === 403;
  return (
    <Card>
      <h3 className={styles.panelTitle}>Permissions</h3>
      <p className={styles.note}>
        This panel reads. Edit a binding on the <Link to="/access/role-bindings">Access page</Link>.
      </p>
      {refused ? (
        // An empty grant list means that no binding reaches the tenant, so a
        // refused caller must never see one.
        <p className={styles.empty}>
          Listing the subjects of a tenant needs the cluster-wide rolebindings read grant.
        </p>
      ) : loading ? (
        <p className={styles.empty}>Reading the grants…</p>
      ) : tenants.length === 0 ? (
        <p className={styles.empty}>This cell holds no tenant, so no binding reaches it.</p>
      ) : (
        (entries ?? []).map((entry) => (
          <section key={entry.tenant} className={styles.tenantBlock}>
            <h4 className={styles.subTitle}>{entry.tenant}</h4>
            <KVGrid
              items={[
                { key: "Jenkins item role", value: <span className={styles.mono}>{entry.itemRole}</span> },
                { key: "Folder pattern", value: <span className={styles.mono}>{entry.itemPattern}</span> },
              ]}
            />
            {entry.grants.length === 0 ? (
              <p className={styles.empty}>
                No binding and no Team membership reaches this tenant. Nobody may build on it.
              </p>
            ) : (
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th>Source</th>
                    <th>Binding</th>
                    <th>Role</th>
                    <th>Subjects</th>
                    <th>Verbs</th>
                    <th>Jobs</th>
                  </tr>
                </thead>
                <tbody>
                  {entry.grants.map((g, i) => (
                    <tr key={`${g.binding ?? "team"}-${i}`}>
                      <td>{g.source}</td>
                      <td className={styles.mono}>{g.binding || DASH}</td>
                      <td>{g.roleRef}</td>
                      <td>{g.subjects.map((s) => `${s.kind}:${s.name}`).join(", ") || DASH}</td>
                      <td>{g.verbs.join(", ") || DASH}</td>
                      <td className={styles.mono}>{g.jobs ? g.jobs.join(", ") : "every job"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {entry.truncated && (
              <p className={styles.note}>
                The list stops at 50 grants for this tenant. The narrowing grants come first.
              </p>
            )}
          </section>
        ))
      )}
    </Card>
  );
}

export default CellTab;
