import styles from "./Pulse.module.css";

interface PulseProps {
  active: boolean;
  size?: number;
  /** Accessible name (also used as a hover title) for screen readers, e.g. "mite connected". */
  label?: string;
}

export function Pulse({ active, size = 11, label }: PulseProps) {
  return (
    <span
      className={`${styles.pulse} ${active ? styles.on : styles.off}`}
      style={{ width: size, height: size }}
      role={label ? "img" : undefined}
      aria-label={label}
      title={label}
    >
      <i />
    </span>
  );
}
