import { type ReactNode } from "react";
import styles from "./Card.module.css";

interface CardProps {
  title?: string;
  headerRight?: ReactNode;
  /** Adds a class to the card root, for a mark on the card edge. */
  className?: string;
  children: ReactNode;
}

export function Card({ title, headerRight, className, children }: CardProps) {
  return (
    <div className={className ? `${styles.card} ${className}` : styles.card}>
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
