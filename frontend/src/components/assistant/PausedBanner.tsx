// PausedBanner tells the viewer that the kill switch is on. The state comes
// from the shared assistant provider.
import { useAssistant } from "./AssistantProvider";

export function PausedBanner() {
  const a = useAssistant();
  if (!a.paused) return null;
  const when = a.pausedAt ? new Date(a.pausedAt).toLocaleString() : "";
  return (
    <p role="status" title={a.pausedAt ?? undefined}>
      The assistant is paused{a.pausedBy ? ` by ${a.pausedBy}` : ""}{when ? ` at ${when}` : ""}.
    </p>
  );
}
