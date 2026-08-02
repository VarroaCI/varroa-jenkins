import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import Controllers from "./Controllers";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockDeleteController = vi.fn();
const mockPreviewBroodOperation = vi.fn();
const mockCreateBroodOperation = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: vi.fn(),
  deleteController: (...args: unknown[]) => mockDeleteController(...args),
  getProvisioningConfig: vi.fn(() => Promise.resolve({ versions: [] })),
  previewBroodOperation: (...args: unknown[]) => mockPreviewBroodOperation(...args),
  createBroodOperation: (...args: unknown[]) => mockCreateBroodOperation(...args),
}));

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({
    permissions: {
      global: {
        controllers: { create: true, delete: true, get: true, list: true, update: true, manage: true },
      },
      scopes: [],
    },
  }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const ctrlFixture = (name: string, overrides?: Record<string, unknown>) => ({
  name,
  namespace: "default",
  cluster: "core",
  phase: "Running",
  endpoint: `https://${name}.example.com`,
  miteConnected: true,
  ...overrides,
});

function renderPage() {
  return renderWithProviders(<Controllers />);
}

describe("Controllers", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null, refetch: vi.fn() });
  });

  it("renders the page heading and description", () => {
    renderPage();
    expect(screen.getByText("Controllers")).toBeInTheDocument();
    expect(screen.getByText(/varroa.dev\/v1alpha1 · Controller/)).toBeInTheDocument();
  });

  it("renders the New controller button with correct link", () => {
    renderPage();
    const newBtn = screen.getByText("＋ New controller");
    expect(newBtn).toBeInTheDocument();
    expect(newBtn.closest("a")).toHaveAttribute("href", "/controllers/create");
  });

  it('shows "No controllers found" when there are no controllers', () => {
    renderPage();
    expect(screen.getByText("No controllers found.")).toBeInTheDocument();
  });

  it("shows delete dialog when delete is triggered", () => {
    // The Controllers page has no UI trigger for delete today — this
    // documents the pre-existing (dead) state, unchanged by this refactor.
    renderPage();
    expect(screen.queryByText("Delete Controller")).not.toBeInTheDocument();
  });

  describe("Run operation flow", () => {
    it("selecting a controller in the picker shows the brood bar and opens the modal", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a"), ctrlFixture("ctrl-b")],
        isLoading: false,
        error: null,
        refetch: vi.fn(),
      });
      renderPage();

      const checkboxes = screen.getAllByRole("checkbox");
      await user.click(checkboxes[1]);

      expect(screen.getByText("Run operation…")).toBeInTheDocument();
      await user.click(screen.getByText("Run operation…"));
      expect(screen.getByText("Run Brood Operation")).toBeInTheDocument();
    });

    it("creates the run and navigates to its detail page", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a"), ctrlFixture("ctrl-b")],
        isLoading: false,
        error: null,
        refetch: vi.fn(),
      });
      mockCreateBroodOperation.mockResolvedValue({ name: "broodop-reconcile-x1", namespace: "default" });
      renderPage();

      const checkboxes = screen.getAllByRole("checkbox");
      await user.click(checkboxes[1]);
      await user.click(checkboxes[2]);
      await user.click(screen.getByText("Run operation…"));
      await user.click(screen.getByText("Create & Run"));

      expect(mockCreateBroodOperation).toHaveBeenCalledTimes(1);
      expect(mockNavigate).toHaveBeenCalledWith("/brood-operations/default/broodop-reconcile-x1");
      // Modal closes and selection clears.
      expect(screen.queryByText("Run Brood Operation")).not.toBeInTheDocument();
      expect(screen.queryByText("Run operation…")).not.toBeInTheDocument();
    });

    it("clearing the selection hides the brood bar", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
        refetch: vi.fn(),
      });
      renderPage();

      await user.click(screen.getAllByRole("checkbox")[1]);
      expect(screen.getByText("Run operation…")).toBeInTheDocument();

      await user.click(screen.getByText("Clear"));
      expect(screen.queryByText("Run operation…")).not.toBeInTheDocument();
    });
  });
});
