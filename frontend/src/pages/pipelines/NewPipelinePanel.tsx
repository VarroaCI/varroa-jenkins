import { useState } from "react";
import { Card } from "../../components/Card";
import ProposePipelineCard from "./ProposePipelineCard";
import styles from "./NewPipelinePanel.module.css";

/**
 * NewPipelinePanel starts a pipeline proposal. It offers the tenants the
 * caller may write, and it shows the proposal form for the chosen one.
 */
export function NewPipelinePanel({ tenants }: { tenants: string[] }) {
  const [chosen, setChosen] = useState("");
  const active = tenants.includes(chosen) ? chosen : (tenants[0] ?? "");

  return (
    <Card title="New pipeline">
      {active === "" ? (
        <p className={styles.note}>No tenant grants you write access to pipelines.</p>
      ) : (
        <>
          <div className={styles.field}>
            <label htmlFor="new-pipeline-tenant" className={styles.label}>
              Tenant
            </label>
            <select
              id="new-pipeline-tenant"
              className={styles.select}
              value={active}
              onChange={(e) => setChosen(e.target.value)}
            >
              {tenants.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          </div>
          <ProposePipelineCard tenant={active} />
        </>
      )}
    </Card>
  );
}
