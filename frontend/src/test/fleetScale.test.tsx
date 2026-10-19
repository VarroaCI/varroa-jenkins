import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Dashboard from "../pages/Dashboard";
import { BroodControllerPicker } from "../components/BroodControllerPicker";
import { renderWithProviders } from "./render-utils";
import { createControllerFleet } from "./factories";
import { stubVirtualViewport } from "./virtualViewport";

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
  useControllerFanoutFailures: () => ({ data: [] }),
}));

vi.mock("../hooks/useApi", () => ({
  bffFetch: vi.fn().mockResolvedValue({ items: [] }),
}));

vi.mock("../api/client", () => ({
  listClusters: vi.fn().mockResolvedValue([]),
  getUpdateCenterStatus: vi.fn().mockResolvedValue({ enabled: false, conditions: [], gaps: [] }),
  getProvisioningConfig: vi.fn(() => Promise.resolve({ versions: [] })),
  listApiKeys: vi.fn().mockResolvedValue({ items: [] }),
  getMyAssistant: vi.fn().mockResolvedValue({ available: false }),
}));

const managePermissions = {
  permissions: {
    global: {
      controllers: { create: true, delete: true, get: true, list: true, update: true, manage: true },
    },
    scopes: [],
  },
};
const mockUseAuth = vi.fn();
vi.mock("../context/AuthContext", () => ({
  useAuth: () => mockUseAuth(),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

function elementCount(container: HTMLElement): number {
  return container.querySelectorAll("*").length;
}

function timed<T>(label: string, fn: () => T): T {
  const t0 = performance.now();
  const out = fn();
  console.info(`[fleet-scale] ${label}: ${(performance.now() - t0).toFixed(1)} ms`);
  return out;
}

describe("fleet scale", () => {
  stubVirtualViewport();

  beforeEach(() => {
    vi.clearAllMocks();
    mockUseAuth.mockReturnValue(managePermissions);
  });

  it("Dashboard renders a bounded brood health card at 400 and 1000 controllers", () => {
    const counts: Record<number, number> = {};
    for (const n of [400, 1000]) {
      mockUseControllers.mockReturnValue({ data: createControllerFleet(n), isLoading: false, error: null, refetch: vi.fn() });
      const { container, unmount } = timed(`Dashboard render n=${n}`, () => renderWithProviders(<Dashboard />));
      counts[n] = elementCount(container);
      const healthRows = container.querySelectorAll("[data-testid='health-row']").length;
      console.info(`[fleet-scale] Dashboard n=${n} elements=${counts[n]} health-rows=${healthRows}`);
      expect(healthRows).toBe(8);
      expect(screen.getByRole("img", { name: /Phase distribution/ })).toBeInTheDocument();
      if (n === 400) {
        expect(screen.getByRole("img", { name: /20 Failed/ })).toBeInTheDocument();
        expect(screen.getByText(/and 392 more/)).toBeInTheDocument();
      } else {
        expect(screen.getByText(/and 992 more/)).toBeInTheDocument();
      }
      unmount();
    }
    expect(counts[1000]).toBeLessThanOrEqual(counts[400] * 1.1);
  });

  it("Controllers list renders a bounded row window at 400 and 1000 controllers", () => {
    const counts: Record<number, number> = {};
    for (const n of [400, 1000]) {
      mockUseControllers.mockReturnValue({ data: createControllerFleet(n), isLoading: false, error: null, refetch: vi.fn() });
      const { container, unmount } = timed(`Controllers render n=${n}`, () =>
        renderWithProviders(<BroodControllerPicker selected={[]} onSelectionChange={vi.fn()} />),
      );
      counts[n] = elementCount(container);
      const rows = screen.getAllByRole("button", { name: /^Controller / });
      console.info(`[fleet-scale] Controllers n=${n} elements=${counts[n]} rendered-rows=${rows.length}`);
      expect(rows.length).toBeLessThanOrEqual(40);
      expect(screen.getByRole("button", { name: /^All \d+$/ })).toHaveTextContent(String(n));
      unmount();
    }
    expect(counts[1000]).toBeLessThanOrEqual(counts[400] * 1.1);
  });

  it("Controllers list select-all selects every filtered controller, not only rendered rows", async () => {
    const onSelectionChange = vi.fn();
    mockUseControllers.mockReturnValue({ data: createControllerFleet(1000), isLoading: false, error: null, refetch: vi.fn() });
    renderWithProviders(<BroodControllerPicker selected={[]} onSelectionChange={onSelectionChange} />);
    const header = screen.getAllByRole("columnheader")[0];
    const checkbox = within(header).queryByRole("checkbox") ?? document.querySelector("input[type='checkbox']");
    await userEvent.click(checkbox as HTMLInputElement);
    expect(onSelectionChange).toHaveBeenCalledTimes(1);
    expect(onSelectionChange.mock.calls[0][0]).toHaveLength(1000);
  });

  it("Controllers list grouped by cluster keeps group headers in the virtual window", async () => {
    mockUseControllers.mockReturnValue({ data: createControllerFleet(400), isLoading: false, error: null, refetch: vi.fn() });
    renderWithProviders(<BroodControllerPicker selected={[]} onSelectionChange={vi.fn()} />);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Group by" }), "cluster");
    expect(screen.getByText("core (360)")).toBeInTheDocument();
  });
});
