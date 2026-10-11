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

function renderPage(entry = "/pipelines/payments/checkout-api%2Fmain/run-1") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[entry]}>
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

describe("BuildPage keyboard shortcut list", () => {
  it("? opens and closes the shortcut list", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "?" });
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeInTheDocument();
    fireEvent.keyDown(document.body, { key: "?" });
    expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).not.toBeInTheDocument();
  });

  it("Escape closes the list and returns focus to the Shortcuts button", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    const shortcuts = screen.getByRole("button", { name: "Shortcuts" });
    fireEvent.click(shortcuts);
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeInTheDocument();
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).not.toBeInTheDocument();
    expect(shortcuts).toHaveFocus();
  });

  it("? closes the list and returns focus to the Shortcuts button", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    const shortcuts = screen.getByRole("button", { name: "Shortcuts" });
    fireEvent.click(shortcuts);
    fireEvent.keyDown(document.body, { key: "?" });
    expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).not.toBeInTheDocument();
    expect(shortcuts).toHaveFocus();
  });

  it("? does nothing while the command palette is open", async () => {
    const palette = document.createElement("div");
    palette.setAttribute("role", "dialog");
    palette.setAttribute("aria-label", "Global search");
    document.body.appendChild(palette);
    try {
      renderPage();
      await screen.findByText(/Build #42/);
      fireEvent.keyDown(document.body, { key: "?" });
      expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).not.toBeInTheDocument();
    } finally {
      palette.remove();
    }
  });

  it("j does nothing while the list is open", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "?" });
    fireEvent.keyDown(document.body, { key: "j" });
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it("j keeps the tab parameter", async () => {
    renderPage("/pipelines/payments/checkout-api%2Fmain/run-1?tab=tests");
    await screen.findByText(/Build #42/);
    fireEvent.keyDown(document.body, { key: "j" });
    expect(mockNavigate).toHaveBeenCalledWith("/pipelines/payments/checkout-api%2Fmain/run-0?tab=tests");
  });

  it("the Older and Newer buttons open the adjacent builds", async () => {
    renderPage();
    await screen.findByText(/Build #42/);
    fireEvent.click(screen.getByRole("button", { name: /Older/ }));
    expect(mockNavigate).toHaveBeenLastCalledWith("/pipelines/payments/checkout-api%2Fmain/run-0");
    fireEvent.click(screen.getByRole("button", { name: /Newer/ }));
    expect(mockNavigate).toHaveBeenLastCalledWith("/pipelines/payments/checkout-api%2Fmain/run-2");
  });

  it("disables Newer on the newest build", async () => {
    mockGetRun.mockResolvedValue({ ...run(), runId: "run-2", buildNumber: 43 });
    renderPage("/pipelines/payments/checkout-api%2Fmain/run-2");
    await screen.findByText(/Build #43/);
    expect(screen.getByRole("button", { name: /Newer/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Older/ })).toBeEnabled();
  });

  it("disables both controls when the run is outside the newest 100 builds", async () => {
    mockGetRun.mockResolvedValue({ ...run(), runId: "run-9", buildNumber: 9 });
    renderPage("/pipelines/payments/checkout-api%2Fmain/run-9");
    await screen.findByText(/Build #9/);
    expect(screen.getByRole("button", { name: /Older/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Newer/ })).toBeDisabled();
  });
});
