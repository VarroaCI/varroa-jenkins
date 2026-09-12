import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import BuildPage from "./BuildPage";
import type { RunDetail } from "../../types";

const mockGetRun = vi.fn();
const mockListRunArtifacts = vi.fn();
const mockListRunTests = vi.fn();
const mockListRunChanges = vi.fn();
const mockListRuns = vi.fn();

vi.mock("../../api/client", async () => {
  const actual = await vi.importActual<typeof import("../../api/client")>("../../api/client");
  return {
    ...actual,
    getRun: (...args: unknown[]) => mockGetRun(...args),
    listRunArtifacts: (...args: unknown[]) => mockListRunArtifacts(...args),
    listRunTests: (...args: unknown[]) => mockListRunTests(...args),
    listRunChanges: (...args: unknown[]) => mockListRunChanges(...args),
    listRuns: (...args: unknown[]) => mockListRuns(...args),
    cancelRun: vi.fn(),
    rerunRun: vi.fn(),
    downloadRunArtifact: vi.fn(),
  };
});

vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: () => ({ lastEvent: null, readyState: "closed", error: null }),
}));

vi.mock("../../components/Toast", () => ({
  useToast: () => ({ toast: vi.fn() }),
}));

vi.mock("../../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: { global: {}, scopes: [] } }),
  canAccessTenantPipeline: () => true,
}));

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ team: "payments", job: "checkout-api/main", run: "run-1" }),
    useNavigate: () => mockNavigate,
  };
});

function run(): RunDetail {
  return {
    runId: "run-1",
    jobPath: "checkout-api/main",
    tenantFolder: "",
    tenant: "payments",
    status: "COMPLETED",
    buildNumber: 42,
    result: "SUCCESS",
    stages: [],
  };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/pipelines/payments/checkout-api%2Fmain/run-1"]}>
        <BuildPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mockGetRun.mockResolvedValue(run());
  mockListRunArtifacts.mockResolvedValue({ items: [] });
  mockListRunTests.mockResolvedValue({ items: [{ suite: "_total", total: 0, failed: 0, skipped: 0 }] });
  mockListRunChanges.mockResolvedValue({ items: [] });
  mockListRuns.mockResolvedValue({
    items: [
      { runId: "run-2", buildNumber: 43 },
      { runId: "run-1", buildNumber: 42 },
      { runId: "run-0", buildNumber: 41 },
    ],
  });
});

describe("BuildPage keyboard shortcuts", () => {
  it("j opens the next older build", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "j" });
    expect(mockNavigate).toHaveBeenCalledWith("/pipelines/payments/checkout-api%2Fmain/run-0");
  });

  it("k opens the next newer build", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "k" });
    expect(mockNavigate).toHaveBeenCalledWith("/pipelines/payments/checkout-api%2Fmain/run-2");
  });

  it("l moves focus to the log viewer", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "l" });
    expect(screen.getByLabelText("Console log")).toHaveFocus();
  });

  it("/ focuses the log search box", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "/" });
    expect(screen.getByLabelText("Search the log")).toHaveFocus();
  });

  it("f toggles follow", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    const followBtn = screen.getByRole("button", { name: "Follow" });
    expect(followBtn).toHaveAttribute("aria-pressed", "true");
    fireEvent.keyDown(document.body, { key: "f" });
    expect(followBtn).toHaveAttribute("aria-pressed", "false");
  });

  it("t toggles timestamps", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    const tsBtn = screen.getByRole("button", { name: "Timestamps" });
    expect(tsBtn).toHaveAttribute("aria-pressed", "false");
    fireEvent.keyDown(document.body, { key: "t" });
    expect(tsBtn).toHaveAttribute("aria-pressed", "true");
  });

  it("does not navigate while focus sits in an input", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    const search = screen.getByLabelText("Search the log");
    search.focus();
    fireEvent.keyDown(search, { key: "j" });
    expect(mockNavigate).not.toHaveBeenCalled();
  });
});
