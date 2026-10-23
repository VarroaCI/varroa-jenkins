// TenantAgentSettings shows the TenantAgent object of one Tenant, read only: its
// spec, every condition, the instructions revision, and the budget bar.
import { Card } from "../../components/Card";
import type { TenantAgent } from "../../types/tenantAgent";
import { revisionHref } from "./forgeLink";
import styles from "./TenantAgentPage.module.css";

/** TenantAgentSettings renders the read-only settings of one Tenant agent. */
export function TenantAgentSettings({ agent }: { agent: TenantAgent }) {
  const limit = agent.dayLimit;
  const pct = limit > 0 ? Math.min(100, Math.round((agent.spendToday / limit) * 100)) : 0;
  // The instructions source is a catalog reference, so the link uses the
  // issue output repo when the object names one.
  const issues = agent.spec.outputs?.issues;
  const sha = agent.status.instructionsSHA ?? "";
  const shaHref = issues && sha ? revisionHref(issues.repo, issues.forge, sha) : null;
  return (
    <div className={styles.groups}>
      <Card title="Object">
        <dl className={styles.facts}>
          <dt>Name</dt>
          <dd>{agent.namespace}/{agent.name}</dd>
          <dt>Enabled</dt>
          <dd>{agent.spec.enabled === false ? "No" : "Yes"}</dd>
          <dt>Provider</dt>
          <dd>{agent.spec.provider || "default"} {agent.spec.model ?? ""}</dd>
          <dt>Data sharing</dt>
          <dd>{agent.spec.dataSharing ?? "default"}</dd>
          <dt>Instructions revision</dt>
          <dd>
            {sha === "" ? "Not read yet" : shaHref ? <a href={shaHref}>{sha.slice(0, 12)}</a> : sha.slice(0, 12)}
          </dd>
        </dl>
      </Card>
      <Card title="Conditions">
        <ul className={styles.list}>
          {(agent.status.conditions ?? []).map((c) => (
            <li key={c.type}>
              <strong>{c.type}</strong> {c.status} · {c.reason}
              {c.message && <div>{c.message}</div>}
            </li>
          ))}
        </ul>
      </Card>
      <Card title="Budget today">
        <div
          role="progressbar"
          aria-label="Spend today against the day limit"
          aria-valuemin={0}
          aria-valuemax={limit}
          aria-valuenow={agent.spendToday}
          className={styles.bar}
        >
          <div className={styles.barFill} style={{ width: `${pct}%` }} />
        </div>
        <p>
          {agent.spendToday} of {limit} tokens
        </p>
      </Card>
    </div>
  );
}
