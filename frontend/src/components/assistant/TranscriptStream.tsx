// TranscriptStream streams one session over SSE and folds its frames into a
// transcript model. It renders nothing. The drawer and the full-page Tenant
// agent transcript both use it.
import { useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useEventStream, type SSEEvent } from "../../hooks/useEventStream";
import { assistantEventsUrl } from "../../api/client";
import { applyEvent, emptyModel, type TranscriptModel } from "./transcript";
import type { AssistantEventType, AssistantFrame } from "../../types/assistant";

export const EVENT_NAMES: AssistantEventType[] = [
  "user", "assistant", "tool_call", "tool_result", "approval_requested", "approval_resolved", "error", "done",
];

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
  const queryClient = useQueryClient();

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
    onModel(next);
    if (frame.type === "assistant" || frame.type === "tool_result") {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
    }
    if (frame.type === "done") {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
      doneRef.current(String(frame.data.state ?? ""));
    }
  };

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

