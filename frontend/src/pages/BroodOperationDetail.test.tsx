import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import BroodOperationDetail from "./BroodOperationDetail";
import { useEventStream } from "../hooks/useEventStream";

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

const mockUseEventStream = vi.mocked(useEventStream);
const mockNavigate = vi.fn();

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => ({ namespace: "test-ns", name: "test-op" }) };
});

const mockGet = vi.fn();

vi.mock("../api/client", () => ({
  getBroodOperation: (...args: unknown[]) => mockGet(...args),
  deleteBroodOperation: vi.fn(),
  suspendBroodOperation: vi.fn(),
  broodStreamUrl: () => "/api/v1/brood-operations/test-ns/test-op/stream",
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
      const { rerender } = renderWithProviders(<BroodOperationDetail />);
      expect(await screen.findByText("Dispatched")).toBeInTheDocument();

      mockUseEventStream.mockReturnValue({
        lastEvent: { type: "status", data: succeededClusterOp },
        readyState: "open",
        error: null,
      });
      rerender(<BroodOperationDetail />);
      const succeededElements = await screen.findAllByText("Succeeded");
      expect(succeededElements.length).toBeGreaterThanOrEqual(1);
      expect(screen.queryByText("Dispatched")).not.toBeInTheDocument();
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
});
