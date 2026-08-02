import styles from "./LoadingSpinner.module.css";

export default function LoadingSpinner() {
  return (
    <div className={styles.spinner}>
      <div className={styles.spin} />
    </div>
  );
}
