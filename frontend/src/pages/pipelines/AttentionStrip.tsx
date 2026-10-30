import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { PipelinesOverview } from "../../types";
import type { FeedQuery } from "./feedQuery";
import styles from "./AttentionStrip.module.css";

/**
 * AttentionStrip lists the counts that need a reader's attention. Each run
 * chip sets a feed filter. The awaiting-cell chip selects its tenant in the
 * rail. The lock chip opens the lock page, or the first tenant that holds a
 * lock for a caller who cannot read the lock page. A count of zero renders
 * no chip.
 */
export function AttentionStrip({
  overview,
  onPreset,
}: {
  overview: PipelinesOverview;
  onPreset: (q: Partial<FeedQuery>) => void;
}) {
  const { attention, tenants } = overview;
  const awaiting = tenants.filter((t) => t.placement === "awaitingCell");
  const withLocks = tenants.find((t) => t.locksHeld > 0);

  const chips: ReactNode[] = [];
  if (attention.failed24h > 0) {
    chips.push(
      <button
        key="failed"
        type="button"
        className={styles.chip}
        onClick={() => onPreset({ result: "FAILURE", since: "24h" })}
      >
        {`${attention.failed24h} failed in the last 24 h`}
      </button>,
    );
  }
  if (attention.waitingInput > 0) {
    const n = attention.waitingInput;
    chips.push(
      <button key="waiting" type="button" className={styles.chip} onClick={() => onPreset({ status: "WAITING_INPUT" })}>
        {n === 1 ? "1 run waits for input" : `${n} runs wait for input`}
      </button>,
    );
  }
  if (attention.awaitingCell > 0 && awaiting.length > 0) {
    const n = attention.awaitingCell;
    chips.push(
      <button
        key="awaiting"
        type="button"
        className={styles.chip}
        onClick={() => onPreset({ tenant: [awaiting[0].name] })}
      >
        {n === 1 ? "1 tenant awaiting a cell" : `${n} tenants awaiting a cell`}
      </button>,
    );
  }
  if (attention.locksHeld > 0) {
    const n = attention.locksHeld;
    const label = n === 1 ? "1 fleet lock held" : `${n} fleet locks held`;
    const to = overview.canReadFleetLocks ? "/locks" : withLocks ? `/pipelines/${encodeURIComponent(withLocks.name)}` : "/pipelines";
    chips.push(
      <Link key="locks" to={to} className={styles.chip}>
        {label}
      </Link>,
    );
  }

  if (chips.length === 0) {
    return <div className={styles.quiet}>Nothing needs attention.</div>;
  }

  return (
    <ul className={styles.strip} aria-label="Needs attention">
      {chips.map((chip, i) => (
        <li key={i} className={styles.item}>
          {chip}
        </li>
      ))}
    </ul>
  );
}
