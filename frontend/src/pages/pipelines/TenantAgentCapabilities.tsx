// TenantAgentCapabilities shows the four capability groups of one Tenant agent:
// reads, writes by approval, unattended, and never.
import { useQuery } from "@tanstack/react-query";
import { getTenantAgentCapabilities } from "../../api/client";
import { Card } from "../../components/Card";
import type { TenantAgentCapability } from "../../types/tenantAgent";
import styles from "./TenantAgentPage.module.css";

const GROUPS: { key: "reads" | "writesByApproval" | "unattended" | "never"; title: string }[] = [
  { key: "reads", title: "Reads" },
  { key: "writesByApproval", title: "Writes by approval" },
  { key: "unattended", title: "Unattended" },
  { key: "never", title: "Never unattended" },
];

/** TenantAgentCapabilities renders the four capability groups. */
export function TenantAgentCapabilities({ tenant }: { tenant: string }) {
  const caps = useQuery({
    queryKey: ["tenant-agent-capabilities", tenant],
    queryFn: () => getTenantAgentCapabilities(tenant),
  });
  if (caps.isPending) return <p>Loading capabilities…</p>;
  if (caps.error) return <p role="alert">The capabilities could not be read.</p>;
  return (
    <div className={styles.groups}>
      {GROUPS.map((g) => (
        <Card key={g.key} title={g.title}>
          <ul className={styles.list} aria-label={g.title}>
            {caps.data[g.key].length === 0 && <li>None</li>}
            {caps.data[g.key].map((c: TenantAgentCapability) => (
              <li key={c.name}>
                <code>{c.name}</code> <span>{c.kind}</span>
                <div>{c.description}</div>
              </li>
            ))}
          </ul>
        </Card>
      ))}
    </div>
  );
}
