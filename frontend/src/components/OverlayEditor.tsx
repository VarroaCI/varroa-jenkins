import type { OverlayResourceKind, OverlayWarning } from "../types";
import styles from "./OverlayEditor.module.css";

export const OVERLAY_RESOURCES: { key: OverlayResourceKind; label: string }[] = [
  { key: "statefulSet", label: "StatefulSet overlay" },
  { key: "service", label: "Service overlay" },
  { key: "ingress", label: "Ingress overlay" },
];

export interface OverlayFieldError {
  field: string;
  message: string;
}

// Render a unified-diff string with +/- line coloring.
export function DiffView({ diff }: { diff: string }) {
  if (!diff.trim()) {
    return <div className={styles.muted}>No changes.</div>;
  }
  return (
    <pre className={styles.overlayDiff}>
      {diff.split("\n").map((line, i) => {
        let cls = "";
        if (line.startsWith("+")) cls = styles.overlayDiffAdd;
        else if (line.startsWith("-")) cls = styles.overlayDiffDel;
        else if (line.startsWith("@@") || line.startsWith("diff ")) cls = styles.overlayDiffMeta;
        return (
          <div key={i} className={cls}>
            {line || " "}
          </div>
        );
      })}
    </pre>
  );
}

interface OverlayEditorProps {
  values: Record<OverlayResourceKind, string>;
  onChange: (key: OverlayResourceKind, value: string) => void;
  podOverridesText: string;
  onPodOverridesChange: (value: string) => void;
  fieldError: OverlayFieldError | null;
  warnings?: OverlayWarning[];
}

// Pure overlay editor: statefulSet/service/ingress patch textareas plus the
// podOverrides block, with inline parse-error surfacing. No network calls —
// callers own preview/save behavior.
export function OverlayEditor({
  values,
  onChange,
  podOverridesText,
  onPodOverridesChange,
  fieldError,
  warnings = [],
}: OverlayEditorProps) {
  return (
    <div>
      {OVERLAY_RESOURCES.map(({ key, label }) => {
        const err = fieldError?.field === key ? fieldError.message : null;
        return (
          <div key={key}>
            <label className={styles.overlayLabel} htmlFor={`overlay-${key}`}>
              {label} (YAML)
            </label>
            <textarea
              id={`overlay-${key}`}
              aria-label={`${label} YAML`}
              className={`${styles.overlayEditor} ${err ? styles.overlayEditorError : ""}`}
              value={values[key]}
              spellCheck={false}
              placeholder={"# strategic-merge patch YAML"}
              onChange={(e) => onChange(key, e.target.value)}
            />
            {err && <p className={styles.overlayFieldError}>⛔ {err}</p>}
          </div>
        );
      })}

      <label className={styles.overlayLabel} htmlFor="overlay-podOverrides">
        podOverrides (YAML)
      </label>
      <textarea
        id="overlay-podOverrides"
        aria-label="podOverrides YAML"
        className={`${styles.overlayEditor} ${
          fieldError?.field === "podOverrides" ? styles.overlayEditorError : ""
        }`}
        value={podOverridesText}
        spellCheck={false}
        placeholder={"env:\n  - name: FOO\n    value: bar"}
        onChange={(e) => onPodOverridesChange(e.target.value)}
      />
      {fieldError?.field === "podOverrides" && (
        <p className={styles.overlayFieldError}>⛔ {fieldError.message}</p>
      )}

      {warnings.length > 0 && (
        <div style={{ marginTop: 12 }}>
          {warnings.map((w, i) => (
            <div key={i} className={styles.overlayWarning}>
              ⚠ <b>{w.resource}</b> {w.path}: {w.message}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
