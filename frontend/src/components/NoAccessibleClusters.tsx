import styles from "./NoAccessibleClusters.module.css";

export default function NoAccessibleClusters() {
  return <div className={styles.empty} role="status">No accessible clusters.</div>;
}
