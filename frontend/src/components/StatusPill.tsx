import type { ControllerPhase } from "../types";
import styles from "./StatusPill.module.css";

interface StatusPillProps {
  phase: ControllerPhase | string;
  size?: "default" | "sm";
}

const phaseClass = (p: string): string => {
  switch (p) {
    case "Connected":
      return styles.connected;
    case "Running":
      return styles.running;
    case "Provisioning":
      return styles.provisioning;
    case "Failed":
      return styles.failed;
    case "Hibernated":
      return styles.hibernated;
    default:
      return styles.pending;
  }
};

export function StatusPill({ phase, size = "default" }: StatusPillProps) {
  return (
    <span className={`${styles.pill} ${phaseClass(phase)} ${size === "sm" ? styles.sm : ""}`}>
      <span className={styles.pdot} />
      {phase}
    </span>
  );
}
