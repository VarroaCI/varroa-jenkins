import type { VersionCatalogEntry } from "../types";
import styles from "./VersionPicker.module.css";

export interface VersionPickerProps {
  versions: VersionCatalogEntry[]; // from /clusters/{cluster}/provisioning/config, version-descending (D)
  value: string; // selected version
  onChange: (v: string) => void;
  disabled?: boolean; // inert opt-out
}

// VersionPicker is the shared version chooser used by the create wizard (step 1)
// and the ControllerDetail Version card. It renders a card grid of catalog
// entries with a recommended badge, channel (LTS/weekly), and an EOL sub-text,
// falling back to a free-text input when the catalog is empty (wizard parity).
export default function VersionPicker({ versions, value, onChange, disabled }: VersionPickerProps) {
  if (!versions || versions.length === 0) {
    return (
      <input
        className={styles.input}
        placeholder="2.462.1"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
        aria-label="Jenkins version"
      />
    );
  }
  return (
    <div className={`${styles.optGrid} ${disabled ? styles.optGridDisabled : ""}`}>
      {versions.map((v) => (
        <div
          key={v.version}
          role="button"
          tabIndex={disabled ? -1 : 0}
          aria-pressed={value === v.version}
          aria-disabled={disabled || undefined}
          className={`${styles.opt} ${value === v.version ? styles.optOn : ""}`}
          onClick={() => {
            if (!disabled) onChange(v.version);
          }}
          onKeyDown={(e) => {
            if (disabled) return;
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              onChange(v.version);
            }
          }}
        >
          {v.recommended && <span className={styles.oBadge}>recommended</span>}
          <span className={styles.oCheck}>&#10003;</span>
          <div className={styles.oName}>{v.version}</div>
          <div className={styles.oSub}>
            {v.channel === "lts" ? "LTS" : "weekly"}
            {v.eol ? ` · EOL ${v.eol}` : ""}
          </div>
        </div>
      ))}
    </div>
  );
}
