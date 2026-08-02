import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import BroodOperations from "./BroodOperations";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockList = vi.fn();
const mockPreviewBroodOperation = vi.fn();
const mockCreateBroodOperation = vi.fn();
vi.mock("../api/client", () => ({
  listBroodOperations: (...args: unknown[]) => mockList(...args),
  deleteBroodOperation: vi.fn(),
  suspendBroodOperation: vi.fn(),
  listClusters: vi.fn(),
  getProvisioningConfig: vi.fn(() => Promise.resolve({ versions: [] })),
  previewBroodOperation: (...args: unknown[]) => mockPreviewBroodOperation(...args),
  createBroodOperation: (...args: unknown[]) => mockCreateBroodOperation(...args),
}));

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

// Admin auth stub
vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({
    permissions: {
      global: { controllers: { read: true, manage: true } },
      scopes: [],
    },
  }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

function renderPage() {
  return renderWithProviders(<BroodOperations />);
}

const runningRow = {
  namespace: "ns1",
  name: "op-running",
  verb: "restart" as const,
  phase: "Running" as const,
  summary: { total: 2, succeeded: 0, failed: 0, skipped: 0 },
  clusters: ["core"],
  startedBy: "admin",
  createdAt: new Date().toISOString(),
};

const succeededRow = {
  namespace: "ns1",
  name: "op-done",
  verb: "reconcile" as const,
  phase: "Succeeded" as const,
  summary: { total: 1, succeeded: 1, failed: 0, skipped: 0 },
  clusters: ["core"],
  startedBy: "admin",
  createdAt: new Date(Date.now() - 3600000).toISOString(),
};

const ctrlFixture = (name: string, overrides?: Record<string, unknown>) => ({
  name,
  namespace: "default",
  cluster: "core",
  phase: "Running",
  endpoint: `https://${name}.example.com`,
  miteConnected: true,
  ...overrides,
});

describe("BroodOperations", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
  });

  it("shows loading text when data is loading", () => {
    mockList.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("shows empty state when no operations exist", async () => {
    mockList.mockResolvedValue({ items: [], clusters: [] });
    renderPage();
		expect(await screen.findByText(/No Brood Operations yet/)).toBeInTheDocument();
  });

  it("renders a table with operation rows", async () => {
    mockList.mockResolvedValue({ items: [runningRow, succeededRow], clusters: [] });
    renderPage();
    expect(await screen.findByText("ns1/op-running")).toBeInTheDocument();
    expect(screen.getByText("ns1/op-done")).toBeInTheDocument();
  });

  it("shows verb, phase, and summary columns", async () => {
    mockList.mockResolvedValue({ items: [runningRow], clusters: [] });
    renderPage();
    expect(await screen.findByText("restart")).toBeInTheDocument();
    expect(screen.getByText("Running")).toBeInTheDocument();
		expect(screen.getByText("0/2 complete")).toBeInTheDocument();
  });

  it("shows Actions column for admin users", async () => {
    mockList.mockResolvedValue({ items: [runningRow], clusters: [] });
    renderPage();
    expect(await screen.findByText("ns1/op-running")).toBeInTheDocument();
    expect(screen.getByText("Actions")).toBeInTheDocument();
  });

  it("hides Suspend/Cancel for terminal Succeeded ops even for admins", async () => {
    mockList.mockResolvedValue({ items: [succeededRow], clusters: [] });
    renderPage();
    expect(await screen.findByText("ns1/op-done")).toBeInTheDocument();
    expect(screen.queryByText("Suspend")).not.toBeInTheDocument();
    expect(screen.queryByText("Cancel")).not.toBeInTheDocument();
  });

  describe("Run operation entry point", () => {
    beforeEach(() => {
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a"), ctrlFixture("ctrl-b")],
        isLoading: false,
        error: null,
      });
      mockList.mockResolvedValue({ items: [], clusters: [] });
    });

    it("shows the Run operation button for admins and opens the wizard", async () => {
      const user = userEvent.setup();
      renderPage();

      await user.click(await screen.findByText("Run operation…"));
      expect(screen.getByText("1. Select controllers")).toBeInTheDocument();
      expect(screen.getByText("ctrl-a")).toBeInTheDocument();
      expect(screen.queryByText("2. Configure & run")).not.toBeInTheDocument();
    });

    it("closes the wizard when Escape is pressed", async () => {
      const user = userEvent.setup();
      renderPage();

      await user.click(await screen.findByText("Run operation…"));
      expect(screen.getByText("1. Select controllers")).toBeInTheDocument();

      await user.keyboard("{Escape}");

      expect(screen.queryByText("1. Select controllers")).not.toBeInTheDocument();
    });

    it("reveals step 2 once a controller is selected", async () => {
      const user = userEvent.setup();
      renderPage();

      await user.click(await screen.findByText("Run operation…"));
      await user.click(screen.getAllByRole("checkbox")[1]);

      expect(screen.getByText("2. Configure & run")).toBeInTheDocument();
      expect(screen.getByText("Preview")).toBeInTheDocument();
    });

    it("creates the run, invalidates the list query, and closes without navigating", async () => {
      const user = userEvent.setup();
      mockCreateBroodOperation.mockResolvedValue({ name: "broodop-restart-x1", namespace: "default" });
      renderPage();

      // Initial mount fetch.
			await screen.findByText("No Brood Operations yet.");
      expect(mockList).toHaveBeenCalledTimes(1);

      await user.click(await screen.findByText("Run operation…"));
      await user.click(screen.getAllByRole("checkbox")[1]);
      await user.click(screen.getByText("Create & Run"));

      expect(mockCreateBroodOperation).toHaveBeenCalledTimes(1);
      expect(mockNavigate).not.toHaveBeenCalledWith(expect.stringContaining("broodop-restart-x1"));
      expect(screen.queryByText("1. Select controllers")).not.toBeInTheDocument();
      // queryClient.invalidateQueries(["brood-operations"]) triggers a refetch.
      await waitFor(() => expect(mockList).toHaveBeenCalledTimes(2));
    });
  });
});
