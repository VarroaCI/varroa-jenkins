// TenantAgentCard shows the Tenant agent of a pipelines Tenant on its home page:
// whether it is enabled, its last session, its pending approvals, and its
// spend today. The card renders nothing when the Tenant has no Tenant agent.
import { useQuery } from "@tanstack/react-query";
import { getTenantAgent } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { Card } from "../../components/Card";
import { Link } from "react-router-dom";
import styles from "./TenantAgentCard.module.css";

/** TenantAgentCard renders the Tenant agent summary of one tenant. */
export default function TenantAgentCard({ tenant }: { tenant: string }) {
  const agent = useQuery({
    queryKey: ["tenant-agent", tenant],
    queryFn: () => getTenantAgent(tenant),
    retry: (count, err) => !(err instanceof ApiError && err.status === 404) && count < 2,
  });
  if (agent.error instanceof ApiError && agent.error.status === 404) return null;
  if (!agent.data) return null;
  const ta = agent.data;
  const last = ta.status.lastSession;
  return (
    <Card
      title="Tenant agent"
      headerRight={<Link to={`/pipelines/${encodeURIComponent(tenant)}/agent`}>Open</Link>}
    >
      <dl className={styles.facts}>
        <dt>Enabled</dt>
        <dd>{ta.spec.enabled === false ? "No" : "Yes"}</dd>
        <dt>Last session</dt>
        <dd>{last ? `${last.state} · ${new Date(last.at).toLocaleString()}` : "None yet"}</dd>
        <dt>Pending approvals</dt>
        <dd>{ta.pendingApprovals}</dd>
        <dt>Spend today</dt>
        <dd>{ta.spendToday} tokens</dd>
      </dl>
    </Card>
  );
}
