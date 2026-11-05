import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, screen, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import BroodOperationDetail from "./BroodOperationDetail";
import { useEventStream, type SSEEvent } from "../hooks/useEventStream";
import type { BroodRun } from "../types";

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

const mockUseEventStream = vi.mocked(useEventStream);

/** STREAM_OPTIONS matches the options object the page passes the status stream. */
const STREAM_OPTIONS = expect.objectContaining({ onEvent: expect.any(Function) });

/** emit delivers each frame through the onEvent the page passed to the status stream, in one act. */
function emit(...frames: SSEEvent<unknown>[]) {
  const calls = mockUseEventStream.mock.calls;
  const onEvent = calls[calls.length - 1]?.[3]?.onEvent;
  if (!onEvent) throw new Error("useEventStream was not called with an onEvent option");
  act(() => {
    for (const frame of frames) onEvent(frame as SSEEvent<BroodRun>);
  });
}
const mockNavigate = vi.fn();

// Mutable route params so tests can simulate navigating between operations
// (the component instance is reused across a param change in the real app).
let mockParams: { namespace: string; name: string } = { namespace: "test-ns", name: "test-op" };

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => mockParams };
});

const mockGet = vi.fn();

vi.mock("../api/client", () => ({
  getBroodOperation: (...args: unknown[]) => mockGet(...args),
  deleteBroodOperation: vi.fn(),
  suspendBroodOperation: vi.fn(),
  broodStreamUrl: (name: string, ns: string) => `/api/v1/brood-operations/${ns}/${name}/stream`,
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({
    permissions: {
      global: { controllers: { read: true, manage: true } },
      scopes: [],
    },
  }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const clusterOp = {
  namespace: "test-ns",
  name: "test-op",
  phase: "Running",
  verb: "reconcile" as const,
  summary: { total: 1, succeeded: 0, failed: 0, skipped: 0 },
  clusters: [
    {
      cluster: "core",
      ok: true,
      op: {
        metadata: { name: "test-op", namespace: "test-ns", creationTimestamp: new Date().toISOString() },
        spec: { action: { verb: "reconcile" }, targets: { names: ["ctrl-a"] } },
        status: {
          phase: "Running",
          summary: { total: 1, succeeded: 0, failed: 0, skipped: 0 },
          targets: [{ namespace: "test-ns", name: "ctrl-a", wave: 0, state: "Dispatched" as const }],
        },
      },
    },
  ],
};

const succeededClusterOp = {
  ...clusterOp,
  phase: "Succeeded" as const,
  clusters: [
    {
      cluster: "core",
      ok: true,
      op: {
        metadata: { name: "test-op", namespace: "test-ns", creationTimestamp: new Date().toISOString(), resourceVersion: "2" },
        spec: { action: { verb: "reconcile" }, targets: { names: ["ctrl-a"] } },
        status: {
          phase: "Succeeded",
          summary: { total: 1, succeeded: 1, failed: 0, skipped: 0 },
          targets: [{ namespace: "test-ns", name: "ctrl-a", wave: 0, state: "Succeeded" as const }],
        },
      },
    },
  ],
};

const suspendedClusterOp = {
  ...clusterOp,
  phase: "Suspended" as const,
};

describe("BroodOperationDetail", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams = { namespace: "test-ns", name: "test-op" };
    mockGet.mockResolvedValue(clusterOp);
    mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
  });

  it("shows loading state initially", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<BroodOperationDetail />);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("renders the operation name and verb", async () => {
    renderWithProviders(<BroodOperationDetail />);
    expect(await screen.findByText("test-ns/test-op")).toBeInTheDocument();
    expect(screen.getByText("reconcile")).toBeInTheDocument();
  });

  it("renders the per-target table with targets from the initial fetch", async () => {
    renderWithProviders(<BroodOperationDetail />);
    expect(await screen.findByText("ctrl-a")).toBeInTheDocument();
    expect(screen.getByText("Dispatched")).toBeInTheDocument();
  });

  it("shows Suspend and Cancel buttons for admin on non-terminal ops", async () => {
    renderWithProviders(<BroodOperationDetail />);
    expect(await screen.findByText("Suspend")).toBeInTheDocument();
    expect(screen.getByText("Cancel")).toBeInTheDocument();
  });

  it("hides Suspend/Cancel once terminal even for admins", async () => {
    mockGet.mockResolvedValue(succeededClusterOp);
    renderWithProviders(<BroodOperationDetail />);
    const succeededElements = await screen.findAllByText("Succeeded");
    expect(succeededElements.length).toBeGreaterThanOrEqual(1);
    expect(screen.queryByText("Suspend")).not.toBeInTheDocument();
    expect(screen.queryByText("Cancel")).not.toBeInTheDocument();
  });

  it("shows Resume (not Suspend) when phase is Suspended", async () => {
    mockGet.mockResolvedValue(suspendedClusterOp);
    renderWithProviders(<BroodOperationDetail />);
    expect(await screen.findByText("Resume")).toBeInTheDocument();
    expect(screen.queryByText("Suspend")).not.toBeInTheDocument();
  });

  describe("SSE live updates", () => {
    it("updates the per-target table when an SSE status event is received", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      // A terminal `status` frame drives the displayed run (and stops the
      // stream); there is no refetch to reconcile with.
      emit({ type: "status", data: succeededClusterOp });
      const succeededElements = await screen.findAllByText("Succeeded");
      expect(succeededElements.length).toBeGreaterThanOrEqual(1);
      expect(screen.queryByText("Dispatched")).not.toBeInTheDocument();
    });

    it("does not let a `closed` frame clobber the last known run", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      // A `closed` frame carries a bare payload, not a BroodRun; it must not
      // replace the live run (which would render a blank page).
      emit({ type: "closed", data: {} as BroodRun });
      expect(screen.getByText("Dispatched")).toBeInTheDocument();
      expect(screen.queryByText("Failed to load brood operation.")).not.toBeInTheDocument();
    });

    it("does not stop the stream (keeps the real baseUrl) after a bare `closed` frame so it can reconnect", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();
      // While the operation is still live the stream stays open.
      expect(mockUseEventStream).toHaveBeenLastCalledWith(
        "/api/v1/brood-operations/test-ns/test-op/stream",
        "broodop:test-ns/test-op",
        ["status", "closed"],
        STREAM_OPTIONS,
      );

      // A bare `closed` frame (no accompanying terminal status) is ambiguous —
      // the operation may still be running server-side. The component must NOT
      // disable the stream: useEventStream keeps the real baseUrl so its own
      // backoff-and-reconnect (mint a fresh ticket on error) picks the stream
      // back up.
      emit({ type: "closed", data: {} as BroodRun });

      expect(mockUseEventStream).toHaveBeenLastCalledWith(
        "/api/v1/brood-operations/test-ns/test-op/stream",
        "broodop:test-ns/test-op",
        ["status", "closed"],
        STREAM_OPTIONS,
      );
    });

    it("stops the stream (null baseUrl) once a status event reports a terminal phase", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      emit({ type: "status", data: succeededClusterOp });

      await waitFor(() =>
        expect(mockUseEventStream).toHaveBeenLastCalledWith(
          null,
          "broodop:test-ns/test-op",
          ["status", "closed"],
          STREAM_OPTIONS,
        ),
      );
    });

    it("shows the terminal status that arrives with the closed frame in one burst, and stops the stream", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      // The BFF sends the terminal status and then the closed frame back to back,
      // so one render can carry both frames.
      emit({ type: "status", data: succeededClusterOp }, { type: "closed", data: {} });
      expect((await screen.findAllByText("Succeeded")).length).toBeGreaterThanOrEqual(1);
      expect(screen.queryByText("Dispatched")).not.toBeInTheDocument();
      await waitFor(() =>
        expect(mockUseEventStream).toHaveBeenLastCalledWith(
          null,
          "broodop:test-ns/test-op",
          ["status", "closed"],
          STREAM_OPTIONS,
        ),
      );
    });

    it("keeps the stream open for a non-terminal status and a deadline closed frame in one burst", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      emit(
        { type: "status", data: clusterOp },
        { type: "closed", data: { reason: "deadline_exceeded" } },
      );

      expect(screen.getByText("Dispatched")).toBeInTheDocument();
      expect(mockUseEventStream).toHaveBeenLastCalledWith(
        "/api/v1/brood-operations/test-ns/test-op/stream",
        "broodop:test-ns/test-op",
        ["status", "closed"],
        STREAM_OPTIONS,
      );
    });

    it("treats a deadline_exceeded `closed` frame as a no-op: stream stays open and the live phase is kept", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      // A live `status` frame reports a non-terminal phase, so the last known
      // live status is Dispatched (not just the page-load snapshot).
      // The stream's one-hour deadline expired while the operation is still
      // running server-side; the `closed` frame carries reason
      // deadline_exceeded. This is ambiguous (the op may still be running), so
      // the component treats it as a no-op: the stream is NOT stopped
      // (useEventStream's own backoff-and-reconnect picks it back up) and the
      // displayed phase stays at the last known non-terminal status — nothing
      // freezes or reverts.
      emit({ type: "closed", data: { reason: "deadline_exceeded" } });

      expect(mockUseEventStream).toHaveBeenLastCalledWith(
        "/api/v1/brood-operations/test-ns/test-op/stream",
        "broodop:test-ns/test-op",
        ["status", "closed"],
        STREAM_OPTIONS,
      );
      expect(screen.getByText("Dispatched")).toBeInTheDocument();
    });

    it("keeps the last live `status` run after a `closed` frame instead of reverting to the initial snapshot", async () => {
      renderWithProviders(<BroodOperationDetail />);
      // Initial fetch: non-terminal (Dispatched) snapshot.
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      // Live `status` frame advances the run to Succeeded; a terminal status
      // also stops the stream.
      emit({ type: "status", data: succeededClusterOp });
      expect((await screen.findAllByText("Succeeded")).length).toBeGreaterThanOrEqual(1);

      // A trailing `closed` frame (bare payload) must NOT revert the displayed
      // run back to the page-load `initial` snapshot — a `closed` frame never
      // touches `lastStatus`, so the UI stays on Succeeded.
      emit({ type: "closed", data: {} as BroodRun });

      expect(screen.getAllByText("Succeeded").length).toBeGreaterThanOrEqual(1);
      expect(screen.queryByText("Dispatched")).not.toBeInTheDocument();
    });

    it("re-opens the stream when navigating to another operation after a terminal close", async () => {
      const { rerender } = renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();
      expect(mockUseEventStream).toHaveBeenLastCalledWith(
        "/api/v1/brood-operations/test-ns/test-op/stream",
        "broodop:test-ns/test-op",
        ["status", "closed"],
        STREAM_OPTIONS,
      );

      // Op A reaches terminal via a genuinely terminal `status` frame ->
      // stopStream sticks (a bare `closed` frame would no longer do this).
      emit({ type: "status", data: succeededClusterOp });
      rerender(<BroodOperationDetail />);
      expect(mockUseEventStream).toHaveBeenLastCalledWith(
        null,
        "broodop:test-ns/test-op",
        ["status", "closed"],
        STREAM_OPTIONS,
      );

      // Navigate to op B in the same component instance (params change).
      mockParams = { namespace: "test-ns-2", name: "test-op-2" };
      rerender(<BroodOperationDetail />);

      // B's stream must reopen with B's URL/scope — not stay permanently
      // closed from A's terminal state.
      await waitFor(() => {
        expect(mockUseEventStream).toHaveBeenLastCalledWith(
          "/api/v1/brood-operations/test-ns-2/test-op-2/stream",
          "broodop:test-ns-2/test-op-2",
          ["status", "closed"],
          STREAM_OPTIONS,
        );
      });
    });
  });

  it("renders collapsible output row when a target has output", async () => {
    const opWithOutput = {
      ...clusterOp,
      clusters: [
        {
          ...clusterOp.clusters[0],
          op: {
            ...clusterOp.clusters[0].op!,
            status: {
              ...clusterOp.clusters[0].op!.status,
              targets: [
                { namespace: "test-ns", name: "ctrl-a", wave: 0, state: "Succeeded" as const, output: "Hello Jenkins!" },
              ],
            },
          },
        },
      ],
    };
    mockGet.mockResolvedValue(opWithOutput);
    renderWithProviders(<BroodOperationDetail />);

    expect(await screen.findByText("Output")).toBeInTheDocument();
    expect(screen.getByText("Hello Jenkins!")).toBeInTheDocument();
  });

  it("does not render output row when target has no output", async () => {
    renderWithProviders(<BroodOperationDetail />);
    expect(await screen.findByText("ctrl-a")).toBeInTheDocument();
    expect(screen.queryByText("Output")).not.toBeInTheDocument();
  });

  describe("whole-operation failure reason", () => {
    function withReason(reason?: string) {
      return {
        ...clusterOp,
        clusters: [
          {
            ...clusterOp.clusters[0],
            op: {
              ...clusterOp.clusters[0].op!,
              status: { ...clusterOp.clusters[0].op!.status, reason },
            },
          },
        ],
      };
    }

    it("renders TargetVersionUnresolved as friendly copy with the raw code in parentheses", async () => {
      mockGet.mockResolvedValue(withReason("TargetVersionUnresolved"));
      renderWithProviders(<BroodOperationDetail />);

      expect(
        await screen.findByText(/The target version or line for this upgrade could not be resolved/),
      ).toBeInTheDocument();
      expect(screen.getByText("(TargetVersionUnresolved)")).toBeInTheDocument();
    });

    it("renders TargetVersionBelowFloor as friendly copy with the raw code in parentheses", async () => {
      mockGet.mockResolvedValue(withReason("TargetVersionBelowFloor"));
      renderWithProviders(<BroodOperationDetail />);

      expect(await screen.findByText(/The target version is below the supported core floor/)).toBeInTheDocument();
      expect(screen.getByText("(TargetVersionBelowFloor)")).toBeInTheDocument();
    });

    it("falls back to the raw code alone for an unmapped reason, with no parenthetical", async () => {
      mockGet.mockResolvedValue(withReason("SomeOtherReason"));
      renderWithProviders(<BroodOperationDetail />);

      expect(await screen.findByText("SomeOtherReason")).toBeInTheDocument();
      expect(screen.queryByText("(SomeOtherReason)")).not.toBeInTheDocument();
    });

    it("renders no Reason line when status.reason is absent", async () => {
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("ctrl-a")).toBeInTheDocument();
      expect(screen.queryByText(/Reason:/)).not.toBeInTheDocument();
    });

    it("leaves per-target reason rendering unaffected", async () => {
      const opWithTargetReason = {
        ...clusterOp,
        clusters: [
          {
            ...clusterOp.clusters[0],
            op: {
              ...clusterOp.clusters[0].op!,
              status: {
                ...clusterOp.clusters[0].op!.status,
                targets: [
                  { namespace: "test-ns", name: "ctrl-a", wave: 0, state: "Failed" as const, reason: "plugin pin conflict: workflow-job" },
                ],
              },
            },
          },
        ],
      };
      mockGet.mockResolvedValue(opWithTargetReason);
      renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("plugin pin conflict: workflow-job")).toBeInTheDocument();
    });
  });
});
