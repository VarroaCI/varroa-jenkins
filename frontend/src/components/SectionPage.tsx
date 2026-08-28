import type { ReactNode } from "react";
import LoadingSpinner from "./LoadingSpinner";
import styles from "./SectionPage.module.css";

interface SectionPageProps {
  title: string;
  description?: string;
  actions?: ReactNode;
  children?: ReactNode;
  loading?: boolean;
  empty?: boolean;
  emptyMessage?: string;
  readOnly?: boolean;
}

export function SectionPage({ title, description, actions, children, loading, empty, emptyMessage = "Nothing to show.", readOnly }: SectionPageProps) {
  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <h1 className={styles.pageTitle}>{title}</h1>
          {description && <p className={styles.pageDesc}>{description}</p>}
        </div>
        {actions && <div>{actions}</div>}
      </div>
      {readOnly && <p role="status" className={styles.pageDesc}>Read-only</p>}
      {loading ? <LoadingSpinner /> : empty ? <p>{emptyMessage}</p> : children}
    </div>
  );
}
