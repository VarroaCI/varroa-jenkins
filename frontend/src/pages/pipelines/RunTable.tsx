import type { ReactNode } from "react";
import tableStyles from "../tables.module.css";
import styles from "./RunTable.module.css";

/** RUN_COLUMNS are the seven columns of a run table, in order. */
export const RUN_COLUMNS = ["Status", "Run", "Tenant", "Actor", "Branch", "Started", "Duration"] as const;

export interface RunTableProps {
  /** The header cells. The plain column names are the default. */
  head?: ReactNode;
  children: ReactNode;
}

/**
 * RunTable is the one frame of a fleet run table. The command center and the
 * Overview both draw their rows inside it.
 */
export function RunTable({ head, children }: RunTableProps) {
  return (
    <div className={tableStyles.tableScroll}>
      <table className={`${tableStyles.dataTable} ${styles.table}`}>
        <thead>
          <tr>{head ?? RUN_COLUMNS.map((label) => <th key={label} scope="col">{label}</th>)}</tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}
