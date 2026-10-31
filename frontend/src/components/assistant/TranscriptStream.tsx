// TranscriptStream streams one session over SSE and folds its frames into a
// transcript model. It renders nothing. The drawer and the full-page Tenant
// agent transcript both use it. It publishes at most one model per animation
// frame, and it marks the model live once the replay burst is quiet.
import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useEventStream, type SSEEvent } from "../../hooks/useEventStream";
import { assistantEventsUrl } from "../../api/client";
import { applyEvent, emptyModel, type TranscriptModel } from "./transcript";
import type { AssistantEventType, AssistantFrame } from "../../types/assistant";

export const EVENT_NAMES: AssistantEventType[] = [
  "user", "assistant", "tool_call", "tool_result", "approval_requested", "approval_resolved", "error", "done",
];

/** BURST_QUIET_MS ends the replay burst of a connection. It stays below the 500 ms loop flush, so the first live chunk animates. */
export const BURST_QUIET_MS = 250;

/** TranscriptStream streams one session and folds its frames into a model. */
export function TranscriptStream({
  sessionId,
  onModel,
  onDone,
  resumeFrom,
}: {
  sessionId: string;
  onModel: (m: TranscriptModel) => void;
  onDone: (state: string) => void;
  resumeFrom: number;
}) {
  const modelRef = useRef<TranscriptModel>({ ...emptyModel(), lastSeq: resumeFrom });
  const doneRef = useRef(onDone);
  doneRef.current = onDone;
  const onModelRef = useRef(onModel);
  onModelRef.current = onModel;
  const frameRef = useRef<number | null>(null);
  const quietRef = useRef<number | null>(null);
  const queryClient = useQueryClient();

  const publishSoon = () => {
    if (frameRef.current !== null) return;
    frameRef.current = requestAnimationFrame(() => {
      frameRef.current = null;
      onModelRef.current(modelRef.current);
    });
  };

  const onEvent = (ev: SSEEvent<unknown>) => {
    const seq = Number(ev.id);
    if (!ev.id || Number.isNaN(seq)) return;
    const frame: AssistantFrame = {
      seq,
      type: ev.type as AssistantEventType,
      data: (ev.data ?? {}) as AssistantFrame["data"],
    };
    const next = applyEvent(modelRef.current, frame);
    if (next === modelRef.current) return;
    modelRef.current = next;
    if (!next.live) {
      if (quietRef.current !== null) window.clearTimeout(quietRef.current);
      quietRef.current = window.setTimeout(() => {
        quietRef.current = null;
        modelRef.current = { ...modelRef.current, live: true };
        publishSoon();
      }, BURST_QUIET_MS);
    }
    publishSoon();
    if (frame.type === "assistant" || frame.type === "tool_result") {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
    }
    if (frame.type === "done") {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
      doneRef.current(String(frame.data.state ?? ""));
    }
  };

  useEffect(() => {
    return () => {
      if (frameRef.current !== null) cancelAnimationFrame(frameRef.current);
      if (quietRef.current !== null) window.clearTimeout(quietRef.current);
      frameRef.current = null;
      quietRef.current = null;
    };
  }, []);

  useEventStream<unknown>(
    assistantEventsUrl(sessionId, resumeFrom),
    `assistant:${sessionId}`,
    EVENT_NAMES,
    {
      onEvent,
      resumeUrl: () => assistantEventsUrl(sessionId, modelRef.current.lastSeq),
    },
  );
  return null;
}
