import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AttentionPanel } from "../pages/overview/AttentionPanel";
import { DayPanel } from "../pages/overview/DayPanel";
import { BroodControllerPicker } from "../components/BroodControllerPicker";
import { renderWithProviders } from "./render-utils";
import { createControllerFleet } from "./factories";
import { stubVirtualViewport } from "./virtualViewport";

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
  useControllerFanoutFailures: () => ({ data: [] }),
}));

vi.mock("../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useApi")>()),
  bffFetch: vi.fn().mockResolvedValue({ items: [] }),
}));

vi.mock("../api/client", () => ({
  listClusters: vi.fn().mockResolvedValue([]),
  getUpdateCenterStatus: vi.fn().mockResolvedValue({ enabled: false, conditions: [], gaps: [] }),
  getPipelinesOverview: vi.fn(() => new Promise(() => {})),
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

  it("Overview attention panel renders at most eight rows at 400 and 1000 controllers", () => {
    const counts: Record<number, number> = {};
    for (const n of [400, 1000]) {
      mockUseControllers.mockReturnValue({ data: createControllerFleet(n), isLoading: false, error: null, refetch: vi.fn() });
      const { container, unmount } = timed(`Attention panel render n=${n}`, () =>
        renderWithProviders(<AttentionPanel canPipelines={false} canControllers isAdmin={false} />),
      );
      counts[n] = elementCount(container);
      const rows = container.querySelectorAll("li");
      console.info(`[fleet-scale] Attention panel n=${n} elements=${counts[n]} rows=${rows.length}`);
      expect(rows.length).toBe(8);
      expect(screen.getByRole("button", { name: /Show all/ })).toBeInTheDocument();
      unmount();
    }
    expect(counts[1000]).toBeLessThanOrEqual(counts[400] * 1.1);
  });

  it("Overview day panel renders one phase bar at 400 and 1000 controllers", () => {
    const counts: Record<number, number> = {};
    for (const n of [400, 1000]) {
      mockUseControllers.mockReturnValue({ data: createControllerFleet(n), isLoading: false, error: null, refetch: vi.fn() });
      const { container, unmount } = timed(`Day panel render n=${n}`, () =>
        renderWithProviders(<DayPanel canPipelines={false} canControllers />),
      );
      counts[n] = elementCount(container);
      expect(container.querySelectorAll("[role='img'][aria-label^='Phase distribution']").length).toBe(1);
      expect(screen.getByRole("img", { name: new RegExp(`${n / 20} Failed`) })).toBeInTheDocument();
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
