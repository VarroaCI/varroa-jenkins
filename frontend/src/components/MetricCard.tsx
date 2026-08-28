import { type ReactNode } from "react";
import styles from "./MetricCard.module.css";

interface MetricCardProps {
  label: string;
  value: string | number;
  sub?: ReactNode;
  icon?: ReactNode;
  accent?: "default" | "ok" | "warn" | "bad" | "info" | "accent" | "honey";
}

const accentClass = (a: string): string => {
  switch (a) {
    case "ok": return styles.softOk;
    case "warn": return styles.softWarn;
    case "bad": return styles.softBad;
    case "info": return styles.softInfo;
    case "accent": return styles.softAccent;
    case "honey": return styles.softHoney;
    default: return "";
  }
};

export function MetricCard({ label, value, sub, icon, accent }: MetricCardProps) {
  return (
    <div className={styles.metric}>
      <div className={styles.mTop}>
        <span className={styles.mLabel}>{label}</span>
        {icon && <span className={`${styles.mIc} ${accent ? accentClass(accent) : ""}`}>{icon}</span>}
      </div>
      <div className={styles.mVal}>{value}</div>
      {sub && <div className={styles.mSub}>{sub}</div>}
    </div>
  );
}
