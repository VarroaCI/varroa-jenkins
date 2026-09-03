import type { ControllerAttention, ControllerAttentionKind, ControllerPhase } from "../types";
import styles from "./StatusPill.module.css";

export const ATTENTION_LABEL: Record<ControllerAttentionKind, string> = {
  failed: "Failed",
  reconcileBlocked: "Blocked",
  bootFailed: "Boot failed",
  pluginRollFailed: "Plugin roll failed",
  applyFailed: "Apply failed",
};

interface StatusPillProps {
  phase: ControllerPhase | string;
  attention?: ControllerAttention;
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

export function StatusPill({ phase, attention, size = "default" }: StatusPillProps) {
  const tone = attention ? styles.failed : phaseClass(phase);
  return (
    <span
      className={`${styles.pill} ${tone} ${size === "sm" ? styles.sm : ""}`}
      title={attention?.message}
    >
      <span className={styles.pdot} />
      {phase}
      {attention && <span className={styles.attention}>{ATTENTION_LABEL[attention.kind]}</span>}
    </span>
  );
}
