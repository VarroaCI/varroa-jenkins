import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { getTeamCell } from "../../api/client";
import { Card } from "../../components/Card";
import { KVGrid } from "../../components/KVGrid";
import { age } from "../../components/activityTimeline.util";
import { controllerRoute } from "../../routing";
import type { TeamCellMember, TeamCellState } from "../../types";
import styles from "./TeamCellCard.module.css";

/** CELL_STATE_LABELS holds the state text the card's badge and the team
 * home's pill both show. One source keeps the two in step. */
export const CELL_STATE_LABELS: Record<Exclude<TeamCellState, "notCellHosted">, string> = {
  placed: "Placed",
  moving: "Moving",
  rehydrating: "Rehydrating",
  unplaced: "Not placed",
};

/** BADGES holds the label and the colour pair of every rendered state. The
 * label is the state signal. Colour supports it and never replaces it,
 * because `moving` and `rehydrating` share one colour. */
const BADGES: Record<Exclude<TeamCellState, "notCellHosted">, { label: string; className: string }> = {
  placed: { label: CELL_STATE_LABELS.placed, className: styles.badgeOk },
  moving: { label: CELL_STATE_LABELS.moving, className: styles.badgeInfo },
  rehydrating: { label: CELL_STATE_LABELS.rehydrating, className: styles.badgeInfo },
  unplaced: { label: CELL_STATE_LABELS.unplaced, className: styles.badgeWarn },
};

/** EXPLANATIONS holds the sentence each state adds under its reason. The
 * `placed` state carries none. */
const EXPLANATIONS: Partial<Record<TeamCellState, string>> = {
  moving: "Varroa is moving this team to another cell. Builds wait until the move ends.",
  rehydrating: "The cell is rebuilding this team's folders. Builds start when the rebuild ends.",
  unplaced: "This team has no cell yet. The reason above names the cause. Ask an operator to act on it.",
};

/** sharingLine counts the other tenants of the cell. It names none of them,
 * because the tenant gate lets no neighbour cross it. */
function sharingLine(count: number): string {
  if (count === 0) return "No other tenant shares this cell.";
  if (count === 1) return "1 other tenant shares this cell.";
  return `${count} other tenants share this cell.`;
}

/** Frame is the landmark. The loading state renders it too, so the
 * `aria-labelledby` target always resolves. */
function Frame({ children }: { children: ReactNode }) {
  return (
    <section className={styles.section} aria-labelledby="team-cell-heading">
      <Card>
        <h2 id="team-cell-heading" className={styles.heading}>
          Your cell
        </h2>
        {children}
      </Card>
    </section>
  );
}

/** MemberCard renders one member of the tenant: the cell it sits on, the
 * role it holds there, and that member's own load. A standby holds the
 * tenant folder and runs no build, so its running count is zero. */
function MemberCard({ member }: { member: TeamCellMember }) {
  return (
    <li className={styles.member}>
      <div className={styles.memberHead}>
        <span className={styles.memberCell}>{member.cell}</span>
        <span className={styles.memberRole}>{member.role}</span>
      </div>
      <div className={styles.load}>
        <Pair label="Running" value={member.runningNow} />
        <Pair label="Last 24h" value={member.runsLast24h} />
        <Pair label="Jobs" value={member.jobs} />
      </div>
    </li>
  );
}

/** Pair is one label and value of the load line, at body size. */
function Pair({ label, value }: { label: string; value: ReactNode }) {
  return (
    <span className={styles.pair}>
      <span className={styles.pairLabel}>{label}</span>
      <span className={styles.pairValue}>{value}</span>
    </span>
  );
}

/**
 * TeamCellCard renders the tenant's own view of the cell that hosts it. It
 * reads `GET /pipelines/{team}/cell` and no other path, and it derives no
 * value the API did not report.
 */
export default function TeamCellCard({ team }: { team: string }) {
  const { data, isError, dataUpdatedAt } = useQuery({
    queryKey: ["team-cell", team],
    queryFn: () => getTeamCell(team),
    enabled: !!team,
    refetchInterval: 30_000,
  });

  // An error hides the card. The team home's own pipelines read owns the
  // page-level recovery state, and the cell is secondary information.
  if (isError) return null;
  if (!data) {
    return (
      <Frame>
        <div className={styles.loading}>Loading cell…</div>
      </Frame>
    );
  }

  const state = data.state;
  if (state === "notCellHosted") return null;

  const badge = BADGES[state];
  const explanation = EXPLANATIONS[state];
  const full = state !== "unplaced";

  const rows: Array<{ key: string; value: ReactNode }> = [];
  if (data.cell) rows.push({ key: "Cell", value: data.cell });
  if (data.cellPool) rows.push({ key: "Pool", value: data.cellPool });
  if (data.profile) rows.push({ key: "Profile", value: data.profile });
  if (data.jenkinsVersion) rows.push({ key: "Jenkins", value: data.jenkinsVersion });

  // The controller page holds its tab in component state and reads no `tab`
  // parameter yet. It therefore opens on its overview tab until C3 wires the
  // parameter.
  const cellHref =
    data.canOpenCell && data.cluster && data.namespace && data.cell
      ? `${controllerRoute(data.cluster, data.namespace, data.cell)}?tab=cell`
      : null;

  return (
    <Frame>
      <div className={styles.badgeRow}>
        <span className={badge.className}>{badge.label}</span>
      </div>

      {data.reason && (
        <div className={styles.reason}>
          <span className={styles.reasonLabel}>Reason</span>
          <span className={styles.reasonValue}>{data.reason}</span>
        </div>
      )}

      {data.warning && (
        <div className={styles.warning} role="alert">
          <span className={styles.warningLabel}>Warning</span>
          <span className={styles.warningReason}>{data.warning.reason}</span>
          <p className={styles.warningMessage}>{data.warning.message}</p>
        </div>
      )}

      {explanation && <p className={styles.explain}>{explanation}</p>}

      {full && (
        <>
          {rows.length > 0 && <KVGrid items={rows} />}

          <div className={styles.load}>
            {data.freeSlots !== undefined && <Pair label="Free slots" value={data.freeSlots} />}
            <Pair label="Queue" value={data.queueLength ?? "not reported"} />
            {data.runningNow !== undefined && (
              <Pair label="Your running builds" value={data.runningNow} />
            )}
          </div>

          {data.members && data.members.length > 0 && (
            <ul className={styles.members} aria-label="Members">
              {data.members.map((member) => (
                <MemberCard key={`${member.namespace}/${member.cell}`} member={member} />
              ))}
            </ul>
          )}

          {data.otherTenantCount !== undefined && (
            <p className={styles.sharing}>{sharingLine(data.otherTenantCount)}</p>
          )}

          <div className={styles.footer}>
            {cellHref && (
              <Link className={styles.link} to={cellHref}>
                Cell details
              </Link>
            )}
            {/* The response carries no heartbeat time, so this age names the
                browser's own fetch. The queue gauge can be older than it. */}
            <span className={styles.updated}>Updated {age(new Date(dataUpdatedAt).toISOString())}</span>
          </div>
        </>
      )}
    </Frame>
  );
}
