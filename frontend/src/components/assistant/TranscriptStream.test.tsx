import { describe, it, expect, vi } from "vitest";
import { act, waitFor, screen } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { installMockEventSource, MockEventSource } from "../../test/mockEventSource";
import { TranscriptStream } from "./TranscriptStream";
import type { TranscriptModel } from "./transcript";

vi.mock("../../hooks/useApi", async (orig) => {
  const actual = await orig<typeof import("../../hooks/useApi")>();
  return {
    ...actual,
    bffFetch: vi.fn(async () => ({ ticket: "vst_test", expiresInSeconds: 30 })),
  };
});

installMockEventSource();

describe("TranscriptStream", () => {
  it("publishes one model for a burst and marks it live after the burst is quiet", async () => {
    const onModel = vi.fn<(m: TranscriptModel) => void>();
    renderWithProviders(
      <TranscriptStream sessionId="s1" resumeFrom={0} onModel={onModel} onDone={() => undefined} />,
    );
    await waitFor(() => expect(MockEventSource.instances.length).toBeGreaterThan(0));
    const es = MockEventSource.instances[MockEventSource.instances.length - 1];

    act(() => {
      es.emitNamedEvent("user", JSON.stringify({ text: "hi", created_at: "2026-10-31T10:00:00Z" }), "1");
      es.emitNamedEvent("assistant", JSON.stringify({ turn: 1, text: "Hel", final: false, created_at: "2026-10-31T10:00:01Z" }), "2");
      es.emitNamedEvent("assistant", JSON.stringify({ turn: 1, text: "lo", final: true, created_at: "2026-10-31T10:00:01Z" }), "3");
    });

    await waitFor(() => expect(onModel).toHaveBeenCalled());
    expect(onModel).toHaveBeenCalledTimes(1);
    const first = onModel.mock.calls[0][0];
    expect(first.live).toBe(false);
    expect(first.rows).toHaveLength(2);
    expect(first.lastSeq).toBe(3);

    await waitFor(() => expect(onModel.mock.calls[onModel.mock.calls.length - 1][0].live).toBe(true));
    expect(screen.queryByText("hi")).toBeNull();
  });
});
