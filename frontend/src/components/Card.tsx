import { type ReactNode } from "react";
import styles from "./Card.module.css";

interface CardProps {
  title?: string;
  headerRight?: ReactNode;
  children: ReactNode;
}

export function Card({ title, headerRight, children }: CardProps) {
  return (
    <div className={styles.card}>
      {(title || headerRight) && (
        <div className={styles.cardHead}>
          {title && <div className={styles.cardTitle}>{title}</div>}
          {headerRight}
        </div>
      )}
      <div className={styles.cardBody}>{children}</div>
    </div>
  );
}
