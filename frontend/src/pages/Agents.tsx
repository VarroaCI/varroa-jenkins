// Agents lists every TeamAgent of the brood: its Team, its cluster, its state,
// its sessions and spend today, and its unattended grant. The unattended
// toggle shows to a holder of teamagents:unattended and asks first.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { listAgents, putAgentUnattended } from "../api/client";
import { usePermissions, canDoGlobal } from "../hooks/usePermissions";
import { Card } from "../components/Card";
import type { TeamAgentItem } from "../types/teamAgent";
import styles from "./Agents.module.css";

/** Agents renders the Team agents of every cluster. */
export default function Agents() {
  const queryClient = useQueryClient();
  const perms = usePermissions().data;
  const canToggle = canDoGlobal(perms, "teamagents", "unattended");
  const list = useQuery({ queryKey: ["team-agents-all"], queryFn: listAgents });

  const toggle = async (item: TeamAgentItem) => {
    const grant = item.unattendedGranted.length === 0;
    const verb = grant ? "Grant" : "Revoke";
    if (!window.confirm(`${verb} unattended writes for ${item.namespace}/${item.name}?`)) return;
    await putAgentUnattended(item.namespace, item.name, grant);
    void queryClient.invalidateQueries({ queryKey: ["team-agents-all"] });
  };

  return (
    <div className={styles.page}>
      <h1 className={styles.title}>Team agents</h1>
      <Card title="All Team agents">
        {list.isPending && <p>Loading Team agents…</p>}
        {list.error && <p role="alert">The Team agents could not be read.</p>}
        {list.data && list.data.items.length === 0 && <p>No Team agents are configured.</p>}
        {list.data && list.data.items.length > 0 && (
          <table className={styles.table}>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Team</th>
                <th scope="col">Cluster</th>
                <th scope="col">State</th>
                <th scope="col">Sessions today</th>
                <th scope="col">Failed today</th>
                <th scope="col">Spend today</th>
                <th scope="col">Unattended</th>
                <th scope="col">Errors</th>
              </tr>
            </thead>
            <tbody>
              {list.data.items.map((item) => (
                <tr key={`${item.namespace}/${item.name}`}>
                  <td>{item.namespace}/{item.name}</td>
                  <td>{item.team}</td>
                  <td>{item.cluster || "-"}</td>
                  <td>
                    {item.paused ? `Paused: ${item.paused.reason}` : item.enabled ? "Active" : "Disabled"}
                  </td>
                  <td>{item.sessionsToday}</td>
                  <td>{item.failedToday}</td>
                  <td>{item.spendToday}</td>
                  <td>
                    {item.unattendedGranted.length > 0 ? `Granted: ${item.unattendedGranted.join(", ")}` : "Not granted"}
                    {canToggle && (
                      <button type="button" onClick={() => void toggle(item)}>
                        {item.unattendedGranted.length > 0 ? "Revoke" : "Grant"}
                      </button>
                    )}
                  </td>
                  <td>
                    {item.conditions.length === 0 ? "None" : item.conditions.map((c) => c.reason).join(", ")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </div>
  );
}
