import type { ReactNode } from "react";
import styles from "./KVGrid.module.css";

interface KVItem {
  key: string;
  value: ReactNode;
}

interface KVGridProps {
  items: KVItem[];
}

export function KVGrid({ items }: KVGridProps) {
  return (
    <div className={styles.kv}>
      {items.map((item, i) => (
        <div className={styles.row} key={i}>
          <div className={styles.k}>{item.key}</div>
          <div className={styles.v}>{item.value}</div>
        </div>
      ))}
    </div>
  );
}
