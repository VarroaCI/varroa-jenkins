import { useState } from "react";
import { Link } from "react-router-dom";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { KVGrid } from "../components/KVGrid";
import { age } from "../components/activityTimeline.util";
import { useCellGrants, useCellPool, useCellUsage } from "../hooks/useCells";
import { useAuth } from "../context/AuthContext";
import { canAccessTenantPipeline, canDoGlobal } from "../hooks/usePermissions";
import { ApiError, isRecordStoreUnreachable } from "../hooks/useApi";
import type {
  CellSignatureRequester,
  ControllerDetail,
} from "../hooks/useControllers";
import type { CellGrant, CellPool, CellTenantGrants, CellUsage } from "../types";
import { splitClusterWideGrants } from "./cellGrants.util";
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
  // The query keeps the last data after a failed refetch. A failed read shows
  // no count, so the panels take no data while an error is set.
  const usageData = usage.error ? undefined : usage.data;

  const cell = ctrl.cell;
  const tenants = cell?.tenants ?? [];
  const applied = new Set(cell?.appliedTenants ?? []);

  return (
    <div className={styles.tab}>
      <TopologyPanel
        cell={cell}
        usage={usageData}
        pool={pool.data}
        canReadPools={canReadPools}
        tenants={tenants}
        applied={applied}
        permissions={permissions}
      />
      <CapacityPanel
        usage={usageData}
        usageError={usage.error}
        onRetry={() => void usage.refetch()}
        pool={pool.data}
        canReadPools={canReadPools}
      />
      <ScriptApprovalsPanel cell={cell} permissions={permissions} />
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
    const readyText = poolReadyLine(pool);
    items.push({
      key: "Pool ready",
      value: poolNotReady(pool) ? <span className={styles.flag}>{readyText}</span> : readyText,
    });
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
        <div className={styles.tableScroll}>
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
        </div>
      )}
    </Card>
  );
}

/**
 * ScriptApprovalsPanel lists the script signatures the sandbox refused in
 * this cell. It is read-only: an operator approves a signature in the pool
 * declaration, never here. The pool approved count sits in the Topology panel.
 */
function ScriptApprovalsPanel({
  cell,
  permissions,
}: {
  cell: CellProjection;
  permissions: Permissions;
}) {
  const pending = cell?.pendingSignatures ?? [];
  const total = cell?.pendingSignaturesTotal ?? pending.length;
  const more = total - pending.length;

  return (
    <Card>
      <h3 className={styles.panelTitle}>Script approvals</h3>
      {pending.length === 0 ? (
        <p className={styles.empty}>No pending signatures.</p>
      ) : (
        <ul className={styles.sigList}>
          {pending.map((p) => (
            <li key={p.signature} className={styles.sigItem}>
              <SignatureCode signature={p.signature} />
              {p.dangerous && <span className={styles.badge}>dangerous</span>}
              {p.requesters && p.requesters.length > 0 && (
                <ul className={styles.requesters}>
                  {p.requesters.map((r, i) => (
                    <li key={`${r.jobPath}-${r.runId ?? ""}-${i}`}>
                      <RequesterLabel requester={r} permissions={permissions} />
                    </li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      )}
      {more > 0 && <p className={styles.note}>+{more} more</p>}
    </Card>
  );
}

function SignatureCode({ signature }: { signature: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(signature);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <span className={styles.sigRow}>
      <code className={styles.sigCode}>{signature}</code>
      <Button variant="ghost" size="sm" aria-label={`Copy signature ${signature}`} onClick={copy}>
        {copied ? "Copied" : "Copy"}
      </Button>
    </span>
  );
}

/**
 * RequesterLabel names one tenant job as tenant / job #build. It links to the
 * build page only when the viewer may read the tenant pipeline and the run id
 * is known.
 */
function RequesterLabel({
  requester,
  permissions,
}: {
  requester: CellSignatureRequester;
  permissions: Permissions;
}) {
  const tenant = requester.tenant ?? "";
  const prefix = tenant ? `${tenant} / ` : "";
  const build = requester.buildNumber ? ` #${requester.buildNumber}` : "";
  const label = `${prefix}${requester.jobPath}${build}`;
  const job = requester.jobPath.startsWith(`${tenant}/`)
    ? requester.jobPath.slice(tenant.length + 1)
    : requester.jobPath;
  const canOpen =
    tenant !== "" &&
    !!requester.runId &&
    canAccessTenantPipeline(permissions, tenant, "read", job);
  if (!canOpen) {
    return <span>{label}</span>;
  }
  const to = `/pipelines/${encodeURIComponent(tenant)}/${encodeURIComponent(job)}/${encodeURIComponent(requester.runId ?? "")}`;
  return <Link to={to}>{label}</Link>;
}

function poolReadyLine(pool?: CellPool): string {
  const condition = pool?.status.conditions?.find((c) => c.type === "PoolReady");
  if (!condition) return DASH;
  const reason = condition.reason ? ` (${condition.reason})` : "";
  return `${condition.status}${reason}`;
}

// poolNotReady is true when the PoolReady condition exists and is not True.
// An Unknown verdict counts as not ready.
function poolNotReady(pool?: CellPool): boolean {
  const condition = pool?.status.conditions?.find((c) => c.type === "PoolReady");
  return condition != null && condition.status !== "True";
}

// poolSignaturesLine counts the signatures the pool approves. It names none
// of them: the list is long, and the pool declaration holds the detail.
function poolSignaturesLine(pool?: CellPool): string {
  if (!pool) return DASH;
  const count = pool.spec.approvedSignatures?.length ?? 0;
  return count === 0 ? "None" : String(count);
}

/** The reason a usage read gave no counts. */
type UsageProblem = "unreachable" | "disabled" | "failed";

/**
 * usageProblem names why a usage read gave no counts. A store that is off
 * and a store that did not answer need different actions.
 */
function usageProblem(error: unknown): UsageProblem | null {
  if (!error) return null;
  if (isRecordStoreUnreachable(error)) return "unreachable";
  if (error instanceof ApiError && error.status === 503) return "disabled";
  return "failed";
}

function CapacityPanel({
  usage,
  usageError,
  onRetry,
  pool,
  canReadPools,
}: {
  usage?: CellUsage;
  usageError: unknown;
  onRetry: () => void;
  pool?: CellPool;
  canReadPools: boolean;
}) {
  const poolCap = pool?.spec.maxTenantsPerCell;
  const capsDiffer = canReadPools && poolCap != null && usage != null && poolCap !== usage.maxTenants;
  const problem = usageProblem(usageError);

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
              ) : problem ? (
                // A failed read does not show whether a gauge exists, so the row shows only the dash.
                <span className={styles.muted}>{DASH}</span>
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
      {problem === "unreachable" ? (
        <div className={styles.empty}>
          The build record store did not answer, so these counts are not available.{" "}
          <Button size="sm" variant="ghost" onClick={onRetry}>
            Retry
          </Button>
        </div>
      ) : problem === "disabled" ? (
        <p className={styles.empty}>The build record store is disabled, so these counts are not available.</p>
      ) : problem === "failed" ? (
        <p className={styles.empty}>The cell usage read failed, so these counts are not available.</p>
      ) : !usage || usage.tenants.length === 0 ? (
        <p className={styles.empty}>No tenant has run a build in the window.</p>
      ) : (
        <div className={styles.tableScroll}>
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
        </div>
      )}
    </Card>
  );
}

function GrantsTable({ grants }: { grants: CellGrant[] }) {
  return (
    <div className={styles.tableScroll}>
      <table className={styles.wideTable}>
        <thead>
          <tr>
            <th>Source</th>
            <th>Binding</th>
            <th>Role</th>
            <th>Subjects</th>
            <th>Verbs</th>
            <th>Jobs</th>
            <th>From</th>
          </tr>
        </thead>
        <tbody>
          {grants.map((g, i) => (
            <tr key={`${g.binding ?? "tenant"}-${i}`}>
              <td>{g.source}</td>
              <td className={styles.mono}>{g.binding || DASH}</td>
              <td className={styles.nowrap}>{g.roleRef}</td>
              <td>{g.subjects.map((s) => `${s.kind}:${s.name}`).join(", ") || DASH}</td>
              <td className={styles.nowrap}>{g.verbs.join(", ") || DASH}</td>
              <td className={styles.mono}>{g.jobs ? g.jobs.join(", ") : "every job"}</td>
              <td className={styles.mono}>{g.federatedFrom?.join(", ") || DASH}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
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
  // A cluster-wide binding reaches every tenant. The BFF repeats its row in
  // each tenant entry, so the panel lists it once above the tenant blocks.
  const split = splitClusterWideGrants(entries ?? []);
  const anyTruncated = (entries ?? []).some((e) => e.truncated);
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
        <>
          {split.clusterWide.length > 0 && (
            <section className={styles.tenantBlock}>
              <h4 className={styles.subTitle}>Cluster-wide bindings</h4>
              <p className={styles.note}>These bindings reach every tenant on this cell.</p>
              <GrantsTable grants={split.clusterWide} />
              {anyTruncated && (
                <p className={styles.note}>
                  The list can be incomplete: a tenant list stops at 50 grants.
                </p>
              )}
            </section>
          )}
          {split.tenants.map(({ entry, own }) => (
          <section key={entry.tenant} className={styles.tenantBlock}>
            <h4 className={styles.subTitle}>{entry.tenant}</h4>
            <KVGrid
              items={[
                { key: "Jenkins item role", value: <span className={styles.mono}>{entry.itemRole}</span> },
                { key: "Folder pattern", value: <span className={styles.mono}>{entry.itemPattern}</span> },
              ]}
            />
            <h5 className={styles.subTitle}>Jenkins member roles</h5>
            {entry.memberRoles.length === 0 ? (
              <p className={styles.empty}>
                No member role exists on this tenant.
              </p>
            ) : (
              <div className={styles.tableScroll}>
                <table className={styles.wideTable}>
                  <thead>
                    <tr>
                      <th>Role</th>
                      <th>Folder pattern</th>
                      <th>Subjects</th>
                      <th>Permissions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {entry.memberRoles.map((r) => (
                      <tr key={r.name}>
                        <td className={styles.mono}>{r.name}</td>
                        <td className={styles.mono}>{r.pattern}</td>
                        <td>{r.subjects.map((s) => `${s.kind}:${s.name}`).join(", ") || DASH}</td>
                        <td className={styles.mono}>{r.permissions.join(", ")}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <p className={styles.note}>
              Scans, runs, replays, restarts, and input answers check the member role.
            </p>
            {entry.grants.length === 0 ? (
              <p className={styles.empty}>
                No binding and no Tenant membership reaches this tenant. Nobody may build on it.
              </p>
            ) : own.length === 0 ? (
              <p className={styles.empty}>
                No grant reaches this tenant alone. The cluster-wide bindings above apply.
              </p>
            ) : (
              <GrantsTable grants={own} />
            )}
            {own.some((g) => g.source === "federatedBinding") && (
              <p className={styles.note}>
                A federatedBinding row is a copy of a core binding on this member cluster.
              </p>
            )}
            {entry.truncated && (
              <p className={styles.note}>
                The list stops at 50 grants for this tenant. The narrowing grants come first.
              </p>
            )}
          </section>
          ))}
        </>
      )}
    </Card>
  );
}

export default CellTab;
