import { runGlyph, runGlyphLabel, type RunGlyph, type RunState } from "./runGlyph";
import styles from "./RunGlyphMark.module.css";

const GLYPH_MARK: Record<RunGlyph, string> = {
  success: "✓",
  failure: "✕",
  unstable: "!",
  aborted: "–",
  interrupted: "‖",
  running: "●",
  queued: "○",
  waiting: "?",
  other: "·",
};

export interface RunGlyphMarkProps {
  run: RunState;
  /** md is the fleet feed size. sm fits a table cell beside a build number. */
  size?: "md" | "sm";
}

/** RunGlyphMark is the one status mark for a run. Every run surface uses it. */
export function RunGlyphMark({ run, size = "md" }: RunGlyphMarkProps) {
  const glyph = runGlyph(run);
  const sizeClass = size === "sm" ? styles.sm : "";
  return (
    <span role="img" aria-label={runGlyphLabel(glyph)} className={`${styles.glyph} ${sizeClass} ${styles[glyph]}`}>
      {GLYPH_MARK[glyph]}
    </span>
  );
}
