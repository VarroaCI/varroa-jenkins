import {
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from "react";
import type { Ref } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Search as SearchIcon } from "lucide-react";
import { runLogUrl } from "../../api/client";
import type {
  RunStage,
  RunLogEndEvent,
  RunLogGapEvent,
  RunLogLineEvent,
  RunLogLiveEvent,
  RunLogMetaEvent,
  RunLogStageEvent,
} from "../../types";
import { parseAnsiLine } from "./ansi";
import type { BodyRow, LogRow, StageInfo } from "./logRows";
import { buildLogRows, stageJumpIds, stageJumpIndex, stickyStage } from "./logRows";
import styles from "./RunLogViewer.module.css";

/** The subset of useEventStream's frame shape RunLogViewer reads. */
export interface RunLogViewerEvent {
  type: string;
  data: unknown;
}

export interface RunLogViewerProps {
  runId: string;
  /**
   * Every log frame the page has received, oldest first. The viewer reads the
   * frames the page hands it, so a frame survives a switch to another tab and
   * back.
   */
  frames: readonly RunLogViewerEvent[];
  /**
   * Counts the frames the page has appended. The viewer applies the frames it
   * has not applied yet when this value changes, so no frame of a burst is
   * lost.
   */
  version: number;
  /** True once the run has stopped, whether or not the log has finalized. */
  runTerminal: boolean;
  /**
   * True when the run record reports the log finalized. The stream sends its
   * end event only on its first poll tick, so the viewer reads this flag to
   * keep a finished log from reading as still being written.
   */
  logFinalized: boolean;
  /**
   * The run record's stage list. The viewer reads it only to resolve a stage
   * with no lines of its own to its descendants.
   */
  stages?: readonly RunStage[];
}

/** Commands the build page issues into the viewer: keyboard shortcuts and graph selection. */
export interface RunLogViewerHandle {
  focusSearch(): void;
  focusViewer(): void;
  toggleFollow(): void;
  toggleTimestamps(): void;
  scrollToStage(stageId: string): void;
}

const NO_STAGES: readonly RunStage[] = [];

function permalinkLine(): number | null {
  const m = /^#L(\d+)$/.exec(window.location.hash);
  return m ? Number(m[1]) : null;
}

function stageFragment(): string | null {
  const m = /^#stage-(.+)$/.exec(window.location.hash);
  if (!m) return null;
  try {
    return decodeURIComponent(m[1]);
  } catch {
    // A malformed percent escape names no stage. The page still renders.
    return null;
  }
}

export function RunLogViewer({
  runId,
  frames,
  version,
  runTerminal,
  logFinalized,
  stages = NO_STAGES,
  ref,
}: RunLogViewerProps & { ref?: Ref<RunLogViewerHandle> }) {
  const [body, setBody] = useState<BodyRow[]>([]);
  const [stageInfo, setStageInfo] = useState<Map<string, StageInfo>>(() => new Map());
  const rows = useMemo<LogRow[]>(() => buildLogRows(body, stageInfo), [body, stageInfo]);
  const [meta, setMeta] = useState<RunLogMetaEvent | null>(null);
  const [live, setLive] = useState(false);
  const [ended, setEnded] = useState<RunLogEndEvent | null>(null);
  const [follow, setFollow] = useState(true);
  // The log is done once the run is terminal and the log is finalized, or
  // once the end frame arrives. A done log has nothing to follow.
  const logDone = runTerminal && (logFinalized || ended !== null);
  const followOn = follow && !logDone;
  const [showTimestamps, setShowTimestamps] = useState(false);
  const [search, setSearch] = useState("");
  const [highlightedLine, setHighlightedLine] = useState<number | null>(null);
  const [pendingLine, setPendingLine] = useState<number | null>(() => permalinkLine());
  // The line the URL fragment names. Only that jump also scrolls the page,
  // because the viewer can sit below the first screen of the build page.
  const urlLineRef = useRef<number | null>(pendingLine);
  // A stage selection waits until a header of that stage, or of one of its
  // descendants, exists. A cold #stage-<id> load starts the wait here.
  const [pendingStage, setPendingStage] = useState<string | null>(() => stageFragment());
  const urlStageRef = useRef<string | null>(pendingStage);

  const scrollRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const lastScrollTopRef = useRef(0);

  // The count of frames this viewer has applied. The count starts at zero,
  // so a remount replays every frame the page holds.
  const appliedRef = useRef(0);
  // The sequences that already have a gap row. The server sends every gap
  // again on each reconnect, so the viewer shows one row per sequence.
  const gapSeqsRef = useRef<Set<number>>(new Set());

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 22,
    overscan: 20,
  });

  // The frames that carry no log row: the manifest, a stage boundary, the
  // live flag, and the closing frame.
  const applyFrame = useCallback((frame: RunLogViewerEvent) => {
    if (frame.type === "meta") {
      setMeta(frame.data as RunLogMetaEvent);
    } else if (frame.type === "live") {
      setLive((frame.data as RunLogLiveEvent).active);
    } else if (frame.type === "end") {
      setEnded(frame.data as RunLogEndEvent);
    }
  }, []);

  // One render carries a whole burst of frames, so the viewer applies every
  // frame it has not applied yet, in order, when the version changes. The
  // count moves to the end of the list, so the next run applies only the
  // frames after it, and a shorter list leaves no stale count behind.
  useEffect(() => {
    const pending = frames.slice(appliedRef.current);
    appliedRef.current = frames.length;

    // A run of line and gap frames becomes one body commit. A remount
    // replays a whole log in one render, and one commit per frame would copy
    // the row list once per frame. A stage frame does not place a header by
    // its position in the stream. It updates the stage names, and the rows
    // place each header before the first line that carries its stage.
    let appended: BodyRow[] = [];
    const stageFrames: RunLogStageEvent[] = [];
    const flush = () => {
      if (appended.length === 0) return;
      const batch = appended;
      appended = [];
      setBody((prev) => [...prev, ...batch]);
    };

    for (const frame of pending) {
      if (frame.type === "line") {
        const ev = frame.data as RunLogLineEvent;
        appended.push({ kind: "line", ...ev });
      } else if (frame.type === "gap") {
        const ev = frame.data as RunLogGapEvent;
        if (gapSeqsRef.current.has(ev.seq)) continue;
        gapSeqsRef.current.add(ev.seq);
        appended.push({ kind: "gap", seq: ev.seq, off: ev.off });
      } else if (frame.type === "stage") {
        stageFrames.push(frame.data as RunLogStageEvent);
      } else {
        applyFrame(frame);
      }
    }
    flush();
    if (stageFrames.length > 0) {
      setStageInfo((prev) => {
        const next = new Map(prev);
        for (const ev of stageFrames) {
          next.set(ev.stageId, {
            name: ev.name || prev.get(ev.stageId)?.name || ev.stageId,
            status: ev.status,
          });
        }
        return next;
      });
    }
  }, [version, frames, applyFrame]);

  // Follow keeps the viewport at the newest line as rows arrive.
  useEffect(() => {
    if (followOn && rows.length > 0) {
      virtualizer.scrollToIndex(rows.length - 1, { align: "end" });
    }
    // virtualizer is a fresh object identity most renders; rows.length and
    // followOn are the only real triggers for this effect.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows.length, followOn]);

  // A pending permalink or search jump resolves once its target row exists.
  useEffect(() => {
    if (pendingLine == null) return;
    const idx = rows.findIndex((r) => r.kind === "line" && r.n === pendingLine);
    if (idx !== -1) {
      // This effect turns follow off for every resolved jump: a permalink,
      // a search hit, or the first error. The scroll events of one frame
      // coalesce, so the scroll handler never sees the jump as a scroll up,
      // and the next row batch would otherwise pull the viewport to the end.
      setFollow(false);
      virtualizer.scrollToIndex(idx, { align: "center" });
      if (urlLineRef.current === pendingLine) {
        scrollRef.current?.scrollIntoView?.({ block: "center" });
      }
      urlLineRef.current = null;
      setHighlightedLine(pendingLine);
      setPendingLine(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows, pendingLine]);

  // A pending stage jump resolves once a header of that stage, or of one of
  // its descendants, exists. A resolved jump turns Follow off, so the next
  // batch of rows does not pull the view to the end.
  useEffect(() => {
    if (pendingStage == null) return;
    const idx = stageJumpIndex(rows, stageJumpIds(pendingStage, stages));
    if (idx === -1) return;
    setFollow(false);
    virtualizer.scrollToIndex(idx, { align: "start" });
    if (urlStageRef.current === pendingStage) {
      scrollRef.current?.scrollIntoView?.({ block: "center" });
    }
    urlStageRef.current = null;
    setPendingStage(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows, pendingStage, stages]);

  const handleScroll = useCallback((e: React.UIEvent<HTMLDivElement>) => {
    const top = e.currentTarget.scrollTop;
    if (top < lastScrollTopRef.current) setFollow(false);
    lastScrollTopRef.current = top;
  }, []);

  const jumpToLine = useCallback((n: number) => {
    urlLineRef.current = null;
    setPendingStage(null);
    setPendingLine(n);
  }, []);

  const runSearch = useCallback(() => {
    if (!search) return;
    const needle = search.toLowerCase();
    const match = rows.find((r) => r.kind === "line" && r.text.toLowerCase().includes(needle));
    if (match && match.kind === "line") jumpToLine(match.n);
  }, [search, rows, jumpToLine]);

  useImperativeHandle(
    ref,
    () => ({
      focusSearch: () => searchRef.current?.focus(),
      focusViewer: () => scrollRef.current?.focus(),
      toggleFollow: () => {
        if (!logDone) setFollow((f) => !f);
      },
      toggleTimestamps: () => setShowTimestamps((t) => !t),
      scrollToStage: (stageId: string) => {
        if (urlStageRef.current !== stageId) urlStageRef.current = null;
        setPendingLine(null);
        setPendingStage(stageId);
      },
    }),
    [logDone],
  );

  // The stage that names the first row of the view. The overscan rows
  // above the view do not count. The virtualizer instance is stable across
  // renders, so this is computed on every render, not memoized: a scroll
  // re-renders the viewer without changing rows or stageInfo.
  const firstViewIndex =
    virtualizer.getVirtualItemForOffset(virtualizer.scrollOffset ?? 0)?.index ??
    virtualizer.getVirtualItems()[0]?.index;
  const stickyHeader =
    firstViewIndex === undefined ? null : stickyStage(rows, firstViewIndex, stageInfo);

  return (
    <div className={styles.wrap}>
      <div className={styles.toolbar}>
        <div className={styles.searchBox}>
          <SearchIcon size={14} aria-hidden="true" />
          <input
            ref={searchRef}
            type="text"
            placeholder="Search"
            aria-label="Search the log"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") runSearch();
            }}
          />
        </div>
        {!!meta?.firstErrorLine && (
          <button type="button" className={styles.errorJump} onClick={() => jumpToLine(meta.firstErrorLine!)}>
            First error · line {meta.firstErrorLine}
          </button>
        )}
        <button
          type="button"
          aria-pressed={showTimestamps}
          className={styles.toggle}
          onClick={() => setShowTimestamps((t) => !t)}
        >
          Timestamps
        </button>
        <button
          type="button"
          aria-pressed={followOn}
          disabled={logDone}
          className={styles.toggle}
          onClick={() => setFollow((f) => !f)}
        >
          Follow
        </button>
        <a
          className={styles.rawLink}
          href={runLogUrl(runId, { raw: true })}
          target="_blank"
          rel="noreferrer"
        >
          Raw
        </a>
        {live && <span className={styles.liveBadge}>Live</span>}
      </div>

      {runTerminal && !logFinalized && meta !== null && !ended && (
        <p className={styles.notice}>The log is still being written.</p>
      )}
      {ended && !ended.logComplete && (
        <div className={styles.incompleteBanner} role="alert">
          The log is incomplete: {ended.incompleteReason || "reason not reported"}.
        </div>
      )}

      {stickyHeader && (
        <div className={styles.stickyHeader}>{stickyHeader.name} — {stickyHeader.status}</div>
      )}

      <div
        ref={scrollRef}
        className={styles.scroll}
        tabIndex={0}
        aria-label="Console log"
        onScroll={handleScroll}
      >
        <div style={{ height: virtualizer.getTotalSize(), width: "100%", position: "relative" }}>
          {virtualizer.getVirtualItems().map((item) => {
            const row = rows[item.index];
            if (!row) return null;
            return (
              <div
                key={item.key}
                data-index={item.index}
                ref={virtualizer.measureElement}
                style={{
                  position: "absolute",
                  top: 0,
                  left: 0,
                  width: "100%",
                  transform: `translateY(${item.start}px)`,
                }}
              >
                {row.kind === "header" && (
                  <div className={styles.header} id={`stage-${row.stageId}`}>
                    {row.name} — {row.status}
                  </div>
                )}
                {row.kind === "gap" && (
                  <div className={styles.gap}>Missing bytes at sequence {row.seq}.</div>
                )}
                {row.kind === "line" && (
                  <div
                    id={`L${row.n}`}
                    className={`${styles.line} ${row.n === highlightedLine ? styles.highlighted : ""}`}
                  >
                    {showTimestamps && (
                      <span className={styles.ts}>{new Date(row.ts).toLocaleTimeString()}</span>
                    )}
                    <span className={styles.lineNo}>{row.n}</span>
                    <span className={styles.text}>
                      {parseAnsiLine(row.text).map((span, i) => (
                        <span
                          key={i}
                          style={{
                            color: span.fg,
                            fontWeight: span.bold ? 700 : undefined,
                            opacity: span.dim ? 0.6 : undefined,
                            textDecoration: span.underline ? "underline" : undefined,
                          }}
                        >
                          {span.text}
                        </span>
                      ))}
                    </span>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
