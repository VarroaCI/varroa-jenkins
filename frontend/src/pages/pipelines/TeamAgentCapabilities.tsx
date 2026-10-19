// TeamAgentCapabilities shows the four capability groups of one Team agent:
// reads, writes by approval, unattended, and never.
import { useQuery } from "@tanstack/react-query";
import { getTeamAgentCapabilities } from "../../api/client";
import { Card } from "../../components/Card";
import type { TeamAgentCapability } from "../../types/teamAgent";
import styles from "./TeamAgentPage.module.css";

const GROUPS: { key: "reads" | "writesByApproval" | "unattended" | "never"; title: string }[] = [
  { key: "reads", title: "Reads" },
  { key: "writesByApproval", title: "Writes by approval" },
  { key: "unattended", title: "Unattended" },
  { key: "never", title: "Never unattended" },
];

/** TeamAgentCapabilities renders the four capability groups. */
export function TeamAgentCapabilities({ team }: { team: string }) {
  const caps = useQuery({
    queryKey: ["team-agent-capabilities", team],
    queryFn: () => getTeamAgentCapabilities(team),
  });
  if (caps.isPending) return <p>Loading capabilities…</p>;
  if (caps.error) return <p role="alert">The capabilities could not be read.</p>;
  return (
    <div className={styles.groups}>
      {GROUPS.map((g) => (
        <Card key={g.key} title={g.title}>
          <ul className={styles.list} aria-label={g.title}>
            {caps.data[g.key].length === 0 && <li>None</li>}
            {caps.data[g.key].map((c: TeamAgentCapability) => (
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
