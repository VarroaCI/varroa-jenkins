import { useState, useMemo, useRef, useCallback } from "react";
import { Link } from "react-router-dom";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useActivityFeed } from "../hooks/useActivityFeed";
import { useClusters, coreOf } from "../hooks/useClusters";
import { controllerRoute } from "../routing";
import {
  type Lane,
  type RenderRow,
  laneFor,
  groupEvents,
  resultMeta,
  passesLane,
  passesSource,
  passesSelection,
  ACTIVITY_TYPE_MAP,
  SOURCE_COLORS,
  SOURCE_LABELS,
  age,
  NEW_PILL_CAP,
} from "./activityTimeline.util";
import type { ActivityEvent, ActivityFilters } from "../types";
import styles from "./ActivityTimeline.module.css";

// ── Props ──────────────────────────────────────────────────────────────────

interface ActivityTimelineProps {
  scope?: { cluster?: string; namespace: string; name: string };
  selectedControllers?: Set<string>;
  hidePicker?: boolean;
  filters?: ActivityFilters;
}

// ── Lane/source chip definitions ───────────────────────────────────────────

const LANE_CHIPS = ["All", "Control plane", "Builds"] as const;
type LaneChip = (typeof LANE_CHIPS)[number];
const LANE_TO_FILTER: Record<LaneChip, "All" | Lane> = {
  All: "All",
  "Control plane": "control",
  Builds: "builds",
};

const SOURCE_CHIPS = ["All", "Operator", "Mite", "User", "Jenkins", "MCP"] as const;
const SOURCE_CHIP_TO_VALUE: Record<string, "All" | string> = {
  All: "All",
  Operator: "operator",
  Mite: "mite",
  User: "user",
  Jenkins: "jenkins",
  MCP: "mcp",
};

// ── Component ──────────────────────────────────────────────────────────────

export default function ActivityTimeline({
  scope,
  selectedControllers,
  hidePicker: _hidePicker,
  filters = {},
}: ActivityTimelineProps) {
  const { data: clusters } = useClusters();
  const core = coreOf(clusters);
  const {
    events,
    pendingCount,
    paused,
    setPaused,
    resume,
    readyState,
    error,
    isLoading,
    hasMore,
    loadMore,
    isLoadingMore,
  } = useActivityFeed(scope, filters);

  const [laneChip, setLaneChip] = useState<LaneChip>("All");
  const [sourceChip, setSourceChip] = useState<string>("All");
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(
    new Set(),
  );

  // ── Scope-filtered events (controller selection applies on activity page) ──
  const scopedEvents = useMemo(() => {
    if (selectedControllers) {
      return events.filter((e) => passesSelection(e, selectedControllers));
    }
    return events;
  }, [events, selectedControllers]);

  // ── Counts per axis (independent of the other axis) ──────────────────────
  const laneCounts = useMemo(() => {
    const counts: Record<string, number> = {
      All: scopedEvents.length,
      control: 0,
      builds: 0,
    };
    for (const e of scopedEvents) {
      const l = laneFor(e);
      counts[l] = (counts[l] ?? 0) + 1;
    }
    return counts;
  }, [scopedEvents]);

  const sourceCounts = useMemo(() => {
    const counts: Record<string, number> = {
      All: scopedEvents.length,
      operator: 0,
      mite: 0,
      user: 0,
      jenkins: 0,
    };
    for (const e of scopedEvents) {
      const s = e.source;
      counts[s] = (counts[s] ?? 0) + 1;
    }
    return counts;
  }, [scopedEvents]);

  // ── Visible events (lane × source AND composition) ──────────────────────
  const activeLane = LANE_TO_FILTER[laneChip];
  const activeSource = SOURCE_CHIP_TO_VALUE[sourceChip];

  const visibleEvents = useMemo(() => {
    return scopedEvents.filter(
      (e) => passesLane(e, activeLane) && passesSource(e, activeSource),
    );
  }, [scopedEvents, activeLane, activeSource]);

  // ── Build groups ─────────────────────────────────────────────────────────
  const renderRows = useMemo(
    () => groupEvents(visibleEvents),
    [visibleEvents],
  );

  // ── Virtualizer ──────────────────────────────────────────────────────────
  const scrollRef = useRef<HTMLDivElement>(null);
  const count = renderRows.length;

  const virtualizer = useVirtualizer({
    count,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 60, // default row height
    overscan: 10,
    measureElement: (el) => {
      // Dynamic measurement for expanded groups
      return el.getBoundingClientRect().height;
    },
  });

  // ── Toggle expansion ─────────────────────────────────────────────────────
  const toggleGroup = useCallback((groupId: string) => {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(groupId)) {
        next.delete(groupId);
      } else {
        next.add(groupId);
      }
      return next;
    });
  }, []);

  // ── Handle resume + scroll to top ───────────────────────────────────────
  const handleResume = useCallback(() => {
    resume();
    virtualizer.scrollToIndex(0);
  }, [resume, virtualizer]);

  // ── Render helpers ───────────────────────────────────────────────────────
  const renderControlRow = (event: ActivityEvent) => {
    const { icon, style } = ACTIVITY_TYPE_MAP[event.type] ?? {
      icon: "·",
      style: "",
    };
    const sourceLabel = SOURCE_LABELS[event.source] ?? event.source;

    return (
      <div className={`${styles.eventRow} ${styles.controlRow}`}>
        <div className={`${styles.laneRail} ${styles.laneRailControl}`} />
        <div className={styles.eventIcon} data-style={style}>
          {icon}
        </div>
        <div className={styles.eventBody}>
          <div className={styles.eventHeader}>
            <span className={styles.eventType}>{event.type}</span>
            <span
              className={styles.sourceChip}
              style={{
                background: SOURCE_COLORS[event.source] ?? "var(--surface-3)",
              }}
            >
              {sourceLabel}
            </span>
            {event.actor && (
              <span className={styles.actor}>{event.actor}</span>
            )}
          </div>
          <div className={styles.eventMessage}>
            {event.controller ? (
              <>
                {event.cluster ? (
                  <Link
                    to={controllerRoute(event.cluster, event.namespace ?? "", event.controller)}
                    className={styles.controllerLink}
                  >
                    {event.controller}
                  </Link>
                ) : (
                  <span className={styles.controllerLink}>{event.controller}</span>
                )}
                {event.namespace && (
                  <span className={styles.namespace}>/{event.namespace}</span>
                )}
                {event.cluster && core && event.cluster !== core.name && (
                  <span className={styles.clusterBadge}>{event.cluster}</span>
                )}
                {" · "}
              </>
            ) : null}
            {event.message}
          </div>
        </div>
        <div
          className={styles.eventTime}
          title={new Date(event.timestamp).toLocaleString()}
        >
          {age(event.timestamp)}
        </div>
      </div>
    );
  };

  const renderBuildRow = (event: ActivityEvent) => {
    const { icon, style } = resultMeta(event);
    const sourceLabel = SOURCE_LABELS[event.source] ?? event.source;

    return (
      <div className={`${styles.eventRow} ${styles.buildRow}`}>
        <div className={`${styles.laneRail} ${styles.laneRailBuilds}`} />
        <div className={styles.eventIcon} data-style={style}>
          {icon}
        </div>
        <div className={styles.eventBody}>
          <div className={styles.eventHeader}>
            {event.buildNumber != null && (
              <span className={styles.buildNumber}>#{event.buildNumber}</span>
            )}
            <span
              className={styles.sourceChip}
              style={{
                background: SOURCE_COLORS[event.source] ?? "var(--surface-3)",
              }}
            >
              {sourceLabel}
            </span>
            {event.result && (
              <span className={styles.resultBadge} data-style={style}>
                {event.result}
              </span>
            )}
          </div>
          <div className={styles.eventMessage}>
            {event.controller ? (
              <>
                {event.cluster ? (
                  <Link
                    to={controllerRoute(event.cluster, event.namespace ?? "", event.controller)}
                    className={styles.controllerLink}
                  >
                    {event.controller}
                  </Link>
                ) : (
                  <span className={styles.controllerLink}>{event.controller}</span>
                )}
                {event.namespace && (
                  <span className={styles.namespace}>/{event.namespace}</span>
                )}
                {event.cluster && core && event.cluster !== core.name && (
                  <span className={styles.clusterBadge}>{event.cluster}</span>
                )}
                {" · "}
              </>
            ) : null}
            {event.message}
            {event.url && (
              <a
                href={event.url}
                target="_blank"
                rel="noreferrer"
                className={styles.buildLink}
              >
                {" "}
                ↗
              </a>
            )}
          </div>
        </div>
        <div
          className={styles.eventTime}
          title={new Date(event.timestamp).toLocaleString()}
        >
          {age(event.timestamp)}
        </div>
      </div>
    );
  };

  const renderBuildGroupRow = (row: RenderRow & { kind: "group" }) => {
    const isExpanded = expandedGroups.has(row.groupId);
    const memberCount = row.members.length;
    const breakdownEntries = Object.entries(row.breakdown).sort(
      ([, a], [, b]) => b - a,
    );

    return (
      <div className={styles.groupContainer}>
        <div
          className={`${styles.groupHeader} ${isExpanded ? styles.groupHeaderExpanded : ""}`}
          onClick={() => toggleGroup(row.groupId)}
          role="button"
          tabIndex={0}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") toggleGroup(row.groupId);
          }}
        >
          <div className={`${styles.laneRail} ${styles.laneRailBuilds}`} />
          <span className={styles.groupToggle}>
            {isExpanded ? "▼" : "▶"}
          </span>
          <div className={styles.groupMeta}>
            <span className={styles.groupController}>
              {row.newest.cluster ? (
                <Link
                  to={controllerRoute(row.newest.cluster, row.newest.namespace ?? "", row.newest.controller ?? "")}
                  onClick={(e) => e.stopPropagation()}
                  className={styles.controllerLink}
                >
                  {row.newest.controller}
                </Link>
              ) : (
                <span className={styles.controllerLink}>{row.newest.controller}</span>
              )}
            </span>
            <span className={styles.groupCount}>{memberCount} builds</span>
            <span className={styles.groupTime}>
              {age(row.newest.timestamp)}
            </span>
          </div>
          <div className={styles.breakdownCluster}>
            {breakdownEntries.map(([result, count]) => {
              const meta = resultMeta({
                result,
              } as ActivityEvent);
              return (
                <span
                  key={result}
                  className={styles.breakdownBadge}
                  data-style={meta.style}
                >
                  {meta.icon} {count}
                </span>
              );
            })}
          </div>
        </div>
        {isExpanded && (
          <div className={styles.groupMembers}>
            {row.members.map((member, idx) => (
              <div key={`${member.timestamp}-${idx}`} className={styles.groupMemberRow}>
                {renderBuildRow(member)}
              </div>
            ))}
          </div>
        )}
      </div>
    );
  };

  // ── Render ────────────────────────────────────────────────────────────────

  return (
    <div className={styles.timeline}>
      {/* Lane selector */}
      <div className={styles.filterBar}>
        <div className={styles.chips}>
          {LANE_CHIPS.map((chip) => {
            const count = chip === "All" ? laneCounts.All : chip === "Control plane" ? laneCounts.control : laneCounts.builds;
            if (chip !== "All" && count === 0) return null;
            return (
            <button
              key={chip}
              className={`${styles.chip} ${laneChip === chip ? styles.chipOn : ""}`}
              onClick={() => setLaneChip(chip)}
            >
              {chip}{" "}
              <span className={styles.chipCount}>{count}</span>
            </button>
            );
          })}
        </div>
        {/* Source filter */}
        <div className={styles.chips}>
          {SOURCE_CHIPS.map((chip) => {
            const count = chip === "All" ? sourceCounts.All : sourceCounts[SOURCE_CHIP_TO_VALUE[chip]] ?? 0;
            if (chip !== "All" && count === 0) return null;
            return (
            <button
              key={chip}
              className={`${styles.chip} ${sourceChip === chip ? styles.chipOn : ""}`}
              onClick={() => setSourceChip(chip)}
            >
              {chip}{" "}
              <span className={styles.chipCount}>{count}</span>
            </button>
            );
          })}
        </div>
      </div>

      {/* Connection indicator */}
      <div className={styles.streamStatus}>
        <span
          className={
            readyState === "open" ? styles.liveDot : styles.deadDot
          }
        />
        {readyState === "open"
          ? "Live"
          : readyState === "connecting"
            ? "Connecting..."
            : "Disconnected"}
        <button
          className={paused ? styles.pauseBtnLive : styles.pauseBtn}
          onClick={() => setPaused(!paused)}
          title={paused ? "Resume live updates" : "Pause to review"}
        >
          {paused ? "▶ Live" : "⏸ Paused"}
        </button>
        {pendingCount > 0 && (
          <button className={styles.newPill} onClick={handleResume}>
            {pendingCount > NEW_PILL_CAP ? "999+" : String(pendingCount)} new
          </button>
        )}
      </div>

      {/* Loading state */}
      {isLoading && (
        <div className={styles.skeletonList}>
          {Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className={styles.skeletonRow}>
              <div className={styles.skeletonIcon} />
              <div className={styles.skeletonText}>
                <div
                  className={styles.skeletonLine}
                  style={{ width: "60%" }}
                />
                <div
                  className={styles.skeletonLine}
                  style={{ width: "40%", height: 10 }}
                />
              </div>
              <div className={styles.skeletonTime} />
            </div>
          ))}
        </div>
      )}

      {/* Error state */}
      {error && !isLoading && (
        <div className={styles.errorBanner}>
          Failed to load activity:{" "}
          {error instanceof Error ? error.message : "Connection error"}
          <button
            className={styles.retryBtn}
            onClick={() => window.location.reload()}
          >
            Retry
          </button>
        </div>
      )}

      {/* Virtualized event list */}
      {!isLoading && !error && (
        <div ref={scrollRef} className={styles.scrollContainer}>
          <div
            style={{
              height: `${virtualizer.getTotalSize()}px`,
              width: "100%",
              position: "relative",
            }}
          >
            {virtualizer.getVirtualItems().map((virtualItem) => {
              const row = renderRows[virtualItem.index];
              if (!row) return null;
              return (
                <div
                  key={
                    row.kind === "group"
                      ? row.groupId
                      : `${row.event.timestamp}-${virtualItem.index}`
                  }
                  data-index={virtualItem.index}
                  ref={virtualizer.measureElement}
                  style={{
                    position: "absolute",
                    top: 0,
                    left: 0,
                    width: "100%",
                    transform: `translateY(${virtualItem.start}px)`,
                  }}
                >
                  {row.kind === "group"
                    ? renderBuildGroupRow(row)
                    : laneFor(row.event) === "builds"
                      ? renderBuildRow(row.event)
                      : renderControlRow(row.event)}
                </div>
              );
            })}
          </div>

          {/* Empty states */}
          {renderRows.length === 0 && scopedEvents.length === 0 && (
            <div className={styles.emptyState}>
              No activity yet. Events will appear when mites connect, controllers
              change phase, or users take action.
            </div>
          )}
          {renderRows.length === 0 && scopedEvents.length > 0 && (
            <div className={styles.emptyState}>
              No events match the current filters.
            </div>
          )}
			{hasMore && (
				<button className={styles.loadMore} onClick={() => void loadMore()} disabled={isLoadingMore}>
					{isLoadingMore ? "Loading history..." : "Load more history"}
				</button>
			)}
        </div>
      )}
    </div>
  );
}
