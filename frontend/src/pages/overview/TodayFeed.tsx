import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { listActivity, listFleetRunGroups } from "../../api/client";
import { ApiError, isRecordStoreUnreachable } from "../../hooks/useApi";
import { runActorText, runHref } from "../../components/runs/runRowText";
import type { ActivityEvent, FleetRunGroup } from "../../types";
import { stripTenantFolder } from "../pipelines/jobPath";
import { hhmm } from "./attentionItems";
import { useNow } from "./useNow";
import {
  FEED_ROWS,
  buildDayFeed,
  eventDetail,
  eventTitle,
  groupDetail,
  groupTitle,
  startOfLocalDay,
  tenantCounts,
  type DayFeed,
  type FoldRow,
  type OtherRow,
  type TimedEventRow,
  type TimedRunRow,
} from "./todayFeed";
import styles from "./TodayFeed.module.css";

export interface TodayFeedProps {
  /** Whether the caller reads pipelines. Without it the feed reads no runs. */
  canPipelines: boolean;
}

/**
 * TodayFeed lists what ran and what changed since local midnight. Runs come
 * from the job groups of the server, with the held failure left out. Events
 * come from the activity read, without build events and routine sign-ins.
 * Each part shows its own state, so a failed read keeps the other part.
 */
export function TodayFeed({ canPipelines }: TodayFeedProps) {
  const now = useNow(60_000);
  const since = startOfLocalDay(now).toISOString();

  const groups = useQuery({
    queryKey: ["pipelines-run-groups", since],
    queryFn: () => listFleetRunGroups({ since }),
    enabled: canPipelines,
    refetchInterval: 60_000,
  });
  const events = useQuery({
    queryKey: ["overview-activity", since],
    queryFn: () => listActivity({ start: since, limit: 100, excludeRoutineSignIns: true, excludeBuildEvents: true }),
    refetchInterval: 30_000,
  });

  const loading = (canPipelines && groups.isPending) || events.isPending;
  const feed = buildDayFeed(canPipelines ? (groups.data?.groups ?? []) : [], events.data?.items ?? []);
  const runsNote = canPipelines ? (groups.isError ? runsErrorNote(groups.error) : undefined) : NOT_READ_NOTE;
  const eventsNote = events.isError ? eventsErrorNote(events.error) : undefined;
  const hasRows = feed.timed.length > 0 || feed.folds.length > 0;
  const empty = !loading && !hasRows && !runsNote && !eventsNote;

  return (
    <section className={styles.panel} aria-labelledby="overview-feed">
      <div className={styles.feedhead}>
        <h2 id="overview-feed" className={styles.title}>
          Earlier today
        </h2>
        <Link to="/activity" className={styles.link}>
          Full activity
        </Link>
      </div>
      {loading && <p className={styles.note}>Loading today&apos;s runs and events…</p>}
      <FeedRows feed={feed} since={since} />
      {canPipelines && groups.data && groups.data.held > 0 && (
        <p className={styles.feednote}>{heldNote(groups.data.held)}</p>
      )}
      {empty && <p className={styles.note}>Nothing ran and nothing changed since midnight.</p>}
      {runsNote && <FeedNote note={runsNote} onRetry={runsNote.retry ? () => void groups.refetch() : undefined} />}
      {groups.data?.more && (
        <p className={styles.feednote}>
          <Link to="/pipelines">More runs are on the Pipelines page.</Link>
        </p>
      )}
      {eventsNote && <FeedNote note={eventsNote} onRetry={() => void events.refetch()} />}
      {events.data?.hasMore && (
        <p className={styles.feednote}>
          <Link to="/activity">More events are on the Activity page.</Link>
        </p>
      )}
    </section>
  );
}

/** heldNote names the failed runs that the list above holds back. */
export function heldNote(held: number): string {
  return held === 1 ? "1 failed run is in the list above." : `${held} failed runs are in the list above.`;
}

interface Note {
  text: string;
  retry: boolean;
}

const NOT_READ_NOTE: Note = { text: "Runs are not listed: you cannot read pipelines.", retry: false };

/**
 * runsErrorNote names why the run groups did not load. An older server that has
 * no groups route answers 404, and that lands in the generic case.
 */
function runsErrorNote(e: unknown): Note {
  if (e instanceof ApiError && e.status === 503) return { text: "Runs are not listed: pipeline data is off.", retry: false };
  if (isRecordStoreUnreachable(e)) return { text: "Runs are not listed: the record store did not answer.", retry: true };
  return { text: "Runs are not listed: Varroa could not load them.", retry: true };
}

/** eventsErrorNote names why the events did not load. */
function eventsErrorNote(e: unknown): Note {
  if (isRecordStoreUnreachable(e)) return { text: "Events are not listed: the record store did not answer.", retry: true };
  return { text: "Events are not listed: Varroa could not load them.", retry: true };
}

function FeedNote({ note, onRetry }: { note: Note; onRetry?: () => void }) {
  return (
    <div className={styles.feednote}>
      <span>{note.text}</span>
      {onRetry && (
        <button type="button" className={styles.retry} onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}

function FeedRows({ feed, since }: { feed: DayFeed; since: string }) {
  const [showAll, setShowAll] = useState(false);
  const [otherOpen, setOtherOpen] = useState(false);
  const timed = showAll ? feed.timed : feed.timed.slice(0, FEED_ROWS);
  return (
    <>
      {timed.map((row) =>
        row.kind === "run" ? <RunItem key={row.key} row={row} /> : <EventItem key={row.key} row={row} />,
      )}
      {feed.timed.length > FEED_ROWS && (
        <div className={styles.more}>
          <button type="button" className={styles.btnGhost} onClick={() => setShowAll((v) => !v)}>
            {showAll ? "Show fewer" : `Show all ${feed.timed.length}`}
          </button>
        </div>
      )}
      {feed.folds.map((row) =>
        row.kind === "fold" ? (
          <FoldItem key={row.key} row={row} since={since} />
        ) : (
          <OtherItem key={row.key} row={row} open={otherOpen} onToggle={() => setOtherOpen((v) => !v)} />
        ),
      )}
    </>
  );
}

interface ItemProps {
  time: string;
  dot?: "ok" | "bad" | "idle";
  title: string;
  detail?: string;
  action?: ReactNode;
  children?: ReactNode;
}

function Item({ time, dot, title, detail, action, children }: ItemProps) {
  return (
    <div className={styles.item}>
      <time className={styles.time}>{time}</time>
      <div className={styles.text}>
        <div className={styles.t}>
          {dot && <span className={styles.dot} data-tone={dot} aria-hidden="true" />}
          {title}
        </div>
        {detail && <div className={styles.d}>{detail}</div>}
        {children}
      </div>
      {action && <div className={styles.action}>{action}</div>}
    </div>
  );
}

function RunItem({ row }: { row: TimedRunRow }) {
  const g = row.group;
  const r = g.newest;
  const dot = g.failed > 0 ? "bad" : g.passed > 0 ? "ok" : "idle";
  const action =
    g.passed === 0 ? (
      <Link className={styles.btn} to={runHref(r.tenant, g.jobPath, r.runId)}>
        Open
      </Link>
    ) : undefined;
  return (
    <Item time={hhmm(row.atIso)} dot={dot} title={groupTitle(g)} detail={groupDetail(g) || undefined} action={action} />
  );
}

function EventItem({ row }: { row: TimedEventRow }) {
  const e: ActivityEvent = row.event;
  const action =
    row.repeats > 1 && e.controller && e.namespace ? (
      <Link className={styles.btn} to={eventLink(e)}>
        Show
      </Link>
    ) : undefined;
  const title = row.repeats > 1 ? `${eventTitle(e)} × ${row.repeats}` : eventTitle(e);
  return <Item time={hhmm(row.atIso)} title={title} detail={eventDetail(e)} action={action} />;
}

/** eventLink names the activity view of one controller. */
function eventLink(e: ActivityEvent): string {
  const qs = new URLSearchParams();
  if (e.cluster) qs.set("cluster", e.cluster);
  qs.set("controller", `${e.namespace ?? ""}/${e.controller ?? ""}`);
  return `/activity?${qs.toString()}`;
}

function FoldItem({ row, since }: { row: FoldRow; since: string }) {
  const g = row.group;
  const other = g.runs - g.passed - g.failed;
  const dot = g.failed > 0 ? "bad" : other > 0 ? "idle" : "ok";
  const qs = new URLSearchParams({ tenant: g.tenant, job: g.jobPath, since });
  return (
    <Item
      time="today"
      dot={dot}
      title={groupTitle(g)}
      detail={groupDetail(g)}
      action={
        <Link className={styles.btn} to={`/pipelines?${qs.toString()}`}>
          Show
        </Link>
      }
    />
  );
}

function OtherItem({ row, open, onToggle }: { row: OtherRow; open: boolean; onToggle: () => void }) {
  return (
    <Item
      time="today"
      dot="ok"
      title={`${row.runs} other runs passed`}
      detail={tenantCounts(row.groups)}
      action={
        <button type="button" className={styles.btn} aria-expanded={open} onClick={onToggle}>
          {open ? "Hide" : "Show"}
        </button>
      }
    >
      {open && <OtherRuns groups={row.groups} />}
    </Item>
  );
}

function OtherRuns({ groups }: { groups: FleetRunGroup[] }) {
  return (
    <ul className={styles.runs}>
      {groups.map((g) => {
        const r = g.newest;
        const build = r.buildNumber !== undefined ? ` #${r.buildNumber}` : "";
        const actor = runActorText(r);
        const job = stripTenantFolder(g.tenantFolder, g.jobPath) || g.jobPath;
        return (
          <li key={r.runId}>
            <Link to={runHref(r.tenant, g.jobPath, r.runId)}>
              {g.tenant} / {job}
              {build}
            </Link>
            {actor ? ` · ${actor}` : ""}
          </li>
        );
      })}
    </ul>
  );
}
