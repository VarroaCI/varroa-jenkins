import { useEffect, useRef, useState } from "react";
import { bffFetch } from "./useApi";

export interface SSEEvent<T> {
  type: string;
  data: T;
  /** The SSE id line of a named frame, when the server sent one. */
  id?: string;
}

/**
 * Options for useEventStream beyond the positional baseUrl/scope/eventNames
 * arguments.
 */
export interface UseEventStreamOptions<T = unknown> {
  /**
   * Called on every reconnect attempt (after an error or a closed
   * connection), never on the first connect. A returned URL replaces
   * baseUrl for that attempt, so a caller that tracks a resume position
   * (a byte offset, a cursor) can reconnect past what it already read
   * instead of replaying the stream from the start. Returning null or
   * omitting the option keeps reconnecting to baseUrl.
   */
  resumeUrl?: () => string | null;

  /**
   * Called for every parsed frame, named or unnamed, after the hook sets
   * lastEvent. A burst of frames arrives between two renders, and React
   * renders once for the whole burst, so lastEvent keeps only the last frame
   * of that burst. A caller that must keep every frame collects them here.
   * The hook calls this option synchronously, so a caller can keep the
   * frames in a ref and count them.
   */
  onEvent?: (ev: SSEEvent<T>) => void;
}

interface StreamTicket {
  ticket: string;
  expiresInSeconds: number;
}

/**
 * useEventStream opens an authenticated Server-Sent-Events stream using the
 * mint-then-connect flow:
 *
 *   1. POST /stream/ticket { scope }  → a short-lived, single-purpose ticket
 *   2. new EventSource(url + "?ticket=" + ticket), url = baseUrl, or
 *      options.resumeUrl() on a reconnect when it returns one
 *   3. on error: close, back off, mint a FRESH ticket, reconnect
 *
 * EventSource's built-in auto-reconnect reuses the original URL, which would
 * replay an already-expired ticket, so it is disabled by closing the stream on
 * error and re-minting. Session tokens are never placed in the URL.
 *
 * Pass baseUrl (without any auth query param) and the stream scope
 * ("brood" | "activity" | "controller:{ns}/{name}"); pass null for either to
 * keep the stream closed.
 *
 * `eventNames` lists the named SSE events to subscribe to via
 * addEventListener (e.g. ["activity"] for the activity stream, ["status",
 * "closed"] for the brood-op stream). EventSource's `onmessage` only fires
 * for default/unnamed frames, so named frames are invisible without these
 * listeners. The controller logs stream emits unnamed `data:` frames and must
 * keep working through `onmessage` — pass no `eventNames` for it.
 *
 * `options.resumeUrl` is optional and defaults to no resume: every existing
 * call site keeps reconnecting to baseUrl unchanged.
 *
 * `options.onEvent` is optional and defaults to nothing. A caller that must
 * keep every frame of a burst passes it, because lastEvent drops all but the
 * last frame of that burst. The hook sets lastEvent first, then calls onEvent.
 * Both options are read through a ref, so a caller can pass a fresh closure on
 * every render without restarting the stream.
 */
export function useEventStream<T>(
  baseUrl: string | null,
  scope: string | null,
  eventNames: string[] = [],
  options: UseEventStreamOptions<T> = {},
): { lastEvent: SSEEvent<T> | null; readyState: string; error: Error | null } {
  const [lastEvent, setLastEvent] = useState<SSEEvent<T> | null>(null);
  const [readyState, setReadyState] = useState<string>("closed");
  const [error, setError] = useState<Error | null>(null);

  // Call sites pass array literals (or rely on the `[]` default), so the
  // array identity changes on every render. Join to a stable key so the effect
  // below does not tear down and re-mint the stream on every render just
  // because a fresh array was passed. SSE event-type names never contain
  // commas, so the join/split round-trips losslessly.
  const eventNamesKey = eventNames.join(",");

  // A fresh options object (and a fresh resumeUrl closure) arrives on every
  // render for the same reason. A ref keeps the effect below from tearing
  // down and re-minting the stream just because a caller re-created the
  // closure; the effect reads the latest closure through the ref instead.
  const resumeUrlRef = useRef(options.resumeUrl);
  resumeUrlRef.current = options.resumeUrl;

  // onEvent gets the same treatment: the frame handlers run outside the
  // render, so they must read the latest closure without the effect below
  // depending on it.
  const onEventRef = useRef(options.onEvent);
  onEventRef.current = options.onEvent;

  useEffect(() => {
    if (!baseUrl || !scope) {
      setReadyState("closed");
      return;
    }

    let es: EventSource | null = null;
    let cancelled = false;
    let backoff = 1000;
    let timer: ReturnType<typeof setTimeout> | null = null;

    // Every parsed frame reaches lastEvent first, and the caller's onEvent
    // second. A throw from onEvent therefore cannot drop the frame from
    // lastEvent.
    const deliver = (ev: SSEEvent<T>) => {
      setLastEvent(ev);
      onEventRef.current?.(ev);
    };

    // A malformed frame carries no event, so it reports null and the caller
    // drops it.
    const parseFrame = (raw: string): T | null => {
      try {
        return JSON.parse(raw) as T;
      } catch {
        return null;
      }
    };

    const scheduleReconnect = () => {
      if (cancelled) return;
      timer = setTimeout(() => connect(true), backoff);
      backoff = Math.min(backoff * 2, 30000);
    };

    async function connect(isReconnect: boolean) {
      if (cancelled) return;
      setReadyState("connecting");
      try {
        const { ticket } = await bffFetch<StreamTicket>("/stream/ticket", {
          method: "POST",
          body: JSON.stringify({ scope }),
        });
        if (cancelled) return;

        // On a reconnect only (never the first connect), a resumeUrl that
        // returns a URL replaces baseUrl for this attempt.
        const resumeTarget = isReconnect ? (resumeUrlRef.current?.() ?? null) : null;
        const target = resumeTarget ?? baseUrl!;
        const sep = target.includes("?") ? "&" : "?";
        es = new EventSource(target + sep + "ticket=" + encodeURIComponent(ticket));

        // Named SSE frames do not fire onmessage (that only handles default /
        // unnamed frames), so register an explicit listener per name.
        const namedEvents = eventNamesKey ? eventNamesKey.split(",") : [];
        for (const name of namedEvents) {
          es.addEventListener(name, (e: MessageEvent) => {
            const data = parseFrame(e.data);
            if (data === null) return;
            deliver({ type: name, data, id: e.lastEventId || undefined });
          });
        }

        es.onopen = () => {
          setReadyState("open");
          setError(null);
          backoff = 1000;
        };
        es.onmessage = (e: MessageEvent) => {
          const ev = parseFrame(e.data) as SSEEvent<T> | null;
          if (ev === null) return;
          deliver(ev);
        };
        es.onerror = () => {
          setReadyState("closed");
          setError(new Error("SSE connection error"));
          es?.close();
          es = null;
          scheduleReconnect();
        };
      } catch (err) {
        setError(err instanceof Error ? err : new Error("failed to open stream"));
        scheduleReconnect();
      }
    }

    void connect(false);

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
      es?.close();
    };
  }, [baseUrl, scope, eventNamesKey]);

  return { lastEvent, readyState, error };
}
