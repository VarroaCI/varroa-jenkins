import type { ReactNode } from "react";
import styles from "./TrendPlot.module.css";

// The plot box. The viewBox keeps these units; CSS scales the box to the card.
export const PLOT_W = 300;
export const PLOT_H = 80;
export const PLOT_PAD = 4;

/**
 * TrendPlot frames one inline SVG chart with a y-scale on its left and the
 * labels of its first and last builds under it. The scale labels sit inside
 * the plot box, so they never spill into the next heading.
 */
export function TrendPlot({
  top,
  bottom,
  first,
  last,
  role,
  ariaLabel,
  inset,
  children,
}: {
  top: string;
  bottom: string;
  first?: string;
  last?: string;
  role: "img" | "group";
  ariaLabel: string;
  /** True when the data sits PAD units inside the box, so the scale labels
   * centre on the data band rather than the box edge. */
  inset?: boolean;
  children: ReactNode;
}) {
  return (
    <div className={styles.frame}>
      <div className={inset ? `${styles.axis} ${styles.inset}` : styles.axis} aria-hidden="true">
        <span data-axis="max">{top}</span>
        <span data-axis="min">{bottom}</span>
      </div>
      <svg
        className={styles.chart}
        viewBox={`0 0 ${PLOT_W} ${PLOT_H}`}
        preserveAspectRatio="none"
        role={role}
        aria-label={ariaLabel}
      >
        {children}
      </svg>
      {first || last ? (
        <>
          <span aria-hidden="true" />
          <div className={styles.ends} aria-hidden="true">
            <span data-end="first">{first}</span>
            <span data-end="last">{last}</span>
          </div>
        </>
      ) : null}
    </div>
  );
}

/**
 * endLabels returns the labels of the first and last slots that hold a
 * value. A slot with no value is skipped, so the label names a build that
 * the chart draws.
 */
export function endLabels<P>(
  slots: { point?: P }[],
  name: (p: P) => string,
  value: (p: P) => string | undefined,
): { first?: string; last?: string } {
  let first: string | undefined;
  let last: string | undefined;
  for (const slot of slots) {
    if (slot.point === undefined) continue;
    const v = value(slot.point);
    if (v === undefined) continue;
    const text = `${name(slot.point)} · ${v}`;
    if (first === undefined) first = text;
    last = text;
  }
  return { first, last };
}
