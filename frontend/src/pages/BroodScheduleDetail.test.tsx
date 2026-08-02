import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import BroodScheduleDetail from "./BroodScheduleDetail";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate, useParams: () => ({ namespace: "team-ns", name: "my-sched" }) };
});

const mockGet = vi.fn();
const mockListOps = vi.fn();
vi.mock("../api/client", () => ({
  getBroodSchedule: (...args: unknown[]) => mockGet(...args),
  listBroodOperations: (...args: unknown[]) => mockListOps(...args),
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

const detailFixture = {
  namespace: "team-ns",
  name: "my-sched",
  spec: {
    schedule: "*/5 * * * *",
    waitForCompletion: true,
    template: {
      targets: { names: ["ctrl-1"] },
      action: { verb: "reconcile" as const },
    },
  },
  status: {
    lastScheduleTime: new Date().toISOString(),
    lastSuccessfulTime: new Date().toISOString(),
    reason: "",
  },
};

describe("BroodScheduleDetail", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows loading state", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    mockListOps.mockResolvedValue({ items: [] });
    renderWithProviders(<BroodScheduleDetail />);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("renders schedule config and status", async () => {
    mockGet.mockResolvedValue(detailFixture);
    mockListOps.mockResolvedValue({ items: [] });
    renderWithProviders(<BroodScheduleDetail />);
    expect(await screen.findByText("team-ns/my-sched")).toBeInTheDocument();
    expect(screen.getByText("*/5 * * * *")).toBeInTheDocument();
    expect(screen.getByText("Yes")).toBeInTheDocument(); // waitForCompletion
  });

  it("shows the waitForCompletion-dependent qualifier for lastSuccessfulTime", async () => {
    mockGet.mockResolvedValue(detailFixture);
    mockListOps.mockResolvedValue({ items: [] });
    renderWithProviders(<BroodScheduleDetail />);
    expect(await screen.findByText(/watch observed no failure/)).toBeInTheDocument();
  });

  it("shows the fire-and-forget qualifier when waitForCompletion is false", async () => {
    mockGet.mockResolvedValue({
      ...detailFixture,
      spec: { ...detailFixture.spec, waitForCompletion: false },
    });
    mockListOps.mockResolvedValue({ items: [] });
    renderWithProviders(<BroodScheduleDetail />);
    expect(await screen.findByText(/trigger job completed/)).toBeInTheDocument();
  });

  it("shows a warning banner when reason is set", async () => {
    mockGet.mockResolvedValue({
      ...detailFixture,
      status: { ...detailFixture.status, reason: "TenancyViolation" },
    });
    mockListOps.mockResolvedValue({ items: [] });
    renderWithProviders(<BroodScheduleDetail />);
    const elements = await screen.findAllByText(/TenancyViolation/, {}, { timeout: 3000 });
    expect(elements.length).toBeGreaterThanOrEqual(1);
  });

  it("renders run history entries", async () => {
    mockGet.mockResolvedValue(detailFixture);
    mockListOps.mockResolvedValue({
      items: [
        { namespace: "team-ns", name: "run-1", verb: "reconcile" as const, phase: "Succeeded" as const, summary: { total: 1, succeeded: 1, failed: 0, skipped: 0 }, clusters: ["core"] },
      ],
    });
    renderWithProviders(<BroodScheduleDetail />);
    expect(await screen.findByText("team-ns/run-1")).toBeInTheDocument();
  });
});
