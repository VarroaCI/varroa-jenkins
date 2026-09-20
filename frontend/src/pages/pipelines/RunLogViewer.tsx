import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Search as SearchIcon } from "lucide-react";
import { runLogUrl } from "../../api/client";
import type {
  RunLogEndEvent,
  RunLogGapEvent,
  RunLogLineEvent,
  RunLogLiveEvent,
  RunLogMetaEvent,
  RunLogStageEvent,
} from "../../types";
import { parseAnsiLine } from "./ansi";
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
}

/** Commands the build page issues into the viewer: keyboard shortcuts and graph selection. */
export interface RunLogViewerHandle {
  focusSearch(): void;
  focusViewer(): void;
  toggleFollow(): void;
  toggleTimestamps(): void;
  scrollToStage(stageId: string): void;
}

type LogRow =
  | { kind: "header"; stageId: string; name: string; status: string }
  | {
      kind: "line";
      n: number;
      off: number;
      ts: number;
      text: string;
      stageId?: string;
      nodeId?: string;
    }
  | { kind: "gap"; seq: number; off: number };

function applyStageEvent(rows: LogRow[], ev: RunLogStageEvent): LogRow[] {
  const idx = rows.findIndex((r) => r.kind === "header" && r.stageId === ev.stageId);
  if (idx === -1) {
    return [...rows, { kind: "header", stageId: ev.stageId, name: ev.name, status: ev.status }];
  }
  const next = rows.slice();
  next[idx] = { kind: "header", stageId: ev.stageId, name: ev.name, status: ev.status };
  return next;
}

function permalinkLine(): number | null {
  const m = /^#L(\d+)$/.exec(window.location.hash);
  return m ? Number(m[1]) : null;
}

export const RunLogViewer = forwardRef<RunLogViewerHandle, RunLogViewerProps>(
  function RunLogViewer({ runId, frames, version, runTerminal }, ref) {
    const [rows, setRows] = useState<LogRow[]>([]);
    const [meta, setMeta] = useState<RunLogMetaEvent | null>(null);
    const [live, setLive] = useState(false);
    const [ended, setEnded] = useState<RunLogEndEvent | null>(null);
    const [follow, setFollow] = useState(true);
    const [showTimestamps, setShowTimestamps] = useState(false);
    const [search, setSearch] = useState("");
    const [highlightedLine, setHighlightedLine] = useState<number | null>(null);
    const [pendingLine, setPendingLine] = useState<number | null>(() => permalinkLine());

    const scrollRef = useRef<HTMLDivElement>(null);
    const searchRef = useRef<HTMLInputElement>(null);
    const lastScrollTopRef = useRef(0);

    // The count of frames this viewer has applied. The count starts at zero,
    // so a remount replays every frame the page holds.
    const appliedRef = useRef(0);

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
      } else if (frame.type === "stage") {
        setRows((prev) => applyStageEvent(prev, frame.data as RunLogStageEvent));
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

      // A run of line and gap frames becomes one rows commit. A remount
      // replays a whole log in one render, and one commit per frame would copy
      // the row list once per frame. A stage frame commits the frames already
      // collected before it, so its header lands after their rows.
      let appended: LogRow[] = [];
      const flush = () => {
        if (appended.length === 0) return;
        const batch = appended;
        appended = [];
        setRows((prev) => [...prev, ...batch]);
      };

      for (const frame of pending) {
        if (frame.type === "line") {
          const ev = frame.data as RunLogLineEvent;
          appended.push({ kind: "line", ...ev });
        } else if (frame.type === "gap") {
          const ev = frame.data as RunLogGapEvent;
          appended.push({ kind: "gap", seq: ev.seq, off: ev.off });
        } else {
          flush();
          applyFrame(frame);
        }
      }
      flush();
    }, [version, frames, applyFrame]);

    // Follow keeps the viewport at the newest line as rows arrive.
    useEffect(() => {
      if (follow && rows.length > 0) {
        virtualizer.scrollToIndex(rows.length - 1, { align: "end" });
      }
      // virtualizer is a fresh object identity most renders; rows.length and
      // follow are the only real triggers for this effect.
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [rows.length, follow]);

    // A pending permalink or search jump resolves once its target row exists.
    useEffect(() => {
      if (pendingLine == null) return;
      const idx = rows.findIndex((r) => r.kind === "line" && r.n === pendingLine);
      if (idx !== -1) {
        virtualizer.scrollToIndex(idx, { align: "center" });
        setHighlightedLine(pendingLine);
        setPendingLine(null);
      }
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [rows, pendingLine]);

    const handleScroll = useCallback((e: React.UIEvent<HTMLDivElement>) => {
      const top = e.currentTarget.scrollTop;
      if (top < lastScrollTopRef.current) setFollow(false);
      lastScrollTopRef.current = top;
    }, []);

    const jumpToLine = useCallback((n: number) => {
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
        toggleFollow: () => setFollow((f) => !f),
        toggleTimestamps: () => setShowTimestamps((t) => !t),
        scrollToStage: (stageId: string) => {
          const idx = rows.findIndex((r) => r.kind === "header" && r.stageId === stageId);
          if (idx !== -1) virtualizer.scrollToIndex(idx, { align: "start" });
        },
      }),
      [rows, virtualizer],
    );

    // The header currently pinned at the top of the viewport, tracked from
    // the first item the virtualizer currently renders.
    const stickyHeader = useMemo(() => {
      const items = virtualizer.getVirtualItems();
      if (items.length === 0) return null;
      for (let i = items[0].index; i >= 0; i--) {
        const r = rows[i];
        if (r?.kind === "header") return r;
      }
      return null;
    }, [virtualizer, rows]);

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
            aria-pressed={follow}
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

        {runTerminal && !ended && (
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
  },
);
