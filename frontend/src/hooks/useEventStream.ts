import { useState, useEffect } from "react";
import { bffFetch } from "./useApi";

export interface SSEEvent<T> {
  type: string;
  data: T;
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
 *   2. new EventSource(baseUrl + "?ticket=" + ticket)
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
 */
export function useEventStream<T>(
  baseUrl: string | null,
  scope: string | null,
  eventNames: string[] = [],
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

  useEffect(() => {
    if (!baseUrl || !scope) {
      setReadyState("closed");
      return;
    }

    let es: EventSource | null = null;
    let cancelled = false;
    let backoff = 1000;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const scheduleReconnect = () => {
      if (cancelled) return;
      timer = setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, 30000);
    };

    async function connect() {
      if (cancelled) return;
      setReadyState("connecting");
      try {
        const { ticket } = await bffFetch<StreamTicket>("/stream/ticket", {
          method: "POST",
          body: JSON.stringify({ scope }),
        });
        if (cancelled) return;

        const sep = baseUrl!.includes("?") ? "&" : "?";
        es = new EventSource(baseUrl! + sep + "ticket=" + encodeURIComponent(ticket));

        // Named SSE frames do not fire onmessage (that only handles default /
        // unnamed frames), so register an explicit listener per name.
        const namedEvents = eventNamesKey ? eventNamesKey.split(",") : [];
        for (const name of namedEvents) {
          es.addEventListener(name, (e: MessageEvent) => {
            try {
              setLastEvent({ type: name, data: JSON.parse(e.data) as T });
            } catch {
              /* ignore malformed frame */
            }
          });
        }

        es.onopen = () => {
          setReadyState("open");
          setError(null);
          backoff = 1000;
        };
        es.onmessage = (e: MessageEvent) => {
          try {
            setLastEvent(JSON.parse(e.data) as SSEEvent<T>);
          } catch {
            /* ignore malformed frame */
          }
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

    void connect();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
      es?.close();
    };
  }, [baseUrl, scope, eventNamesKey]);

  return { lastEvent, readyState, error };
}
