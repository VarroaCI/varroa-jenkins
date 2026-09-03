import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useLocation } from "react-router-dom";
import { renderWithProviders } from "../test/render-utils";
import { BroodControllerPicker } from "./BroodControllerPicker";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

vi.mock("../api/client", () => ({
  listClusters: vi.fn(),
  getProvisioningConfig: vi.fn(() => Promise.resolve({ versions: [] })),
}));

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
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
mockUseAuth.mockReturnValue(managePermissions);
vi.mock("../context/AuthContext", () => ({
  useAuth: () => mockUseAuth(),
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

function renderPicker(onSelectionChange = vi.fn(), compact = false) {
  return renderWithProviders(
    <BroodControllerPicker selected={[]} onSelectionChange={onSelectionChange} compact={compact} />,
  );
}

describe("BroodControllerPicker", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    mockUseAuth.mockReturnValue(managePermissions);
  });

  describe("Loading state", () => {
    it("shows a loading banner when controllers are loading", () => {
      mockUseControllers.mockReturnValue({ data: null, isLoading: true, error: null });
      renderPicker();
      expect(screen.getByText("Loading controllers...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner when loading fails", () => {
      mockUseControllers.mockReturnValue({ data: null, isLoading: false, error: { message: "API unavailable" } });
      renderPicker();
      expect(screen.getByText(/Failed to load: API unavailable/)).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it('shows "No controllers found" when there are no controllers', () => {
      mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
      renderPicker();
      expect(screen.getByText("No controllers found.")).toBeInTheDocument();
    });
  });

  describe("Happy path", () => {
    it("renders controller rows with name, phase, endpoint, mite status", () => {
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a"),
          ctrlFixture("ctrl-b", { phase: "Provisioning", endpoint: "", miteConnected: false }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      expect(screen.getByText("ctrl-a")).toBeInTheDocument();
      expect(screen.getByText("ctrl-b")).toBeInTheDocument();
      expect(screen.getAllByText("Provisioning").length).toBeGreaterThanOrEqual(1);
    });
  });

  describe("Version column (#242)", () => {
    it("shows the running version in place of the dash", () => {
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a", { jenkinsVersion: "2.555.1", version: "2.555" })],
        isLoading: false,
        error: null,
      });
      renderPicker();
      expect(screen.getByText("2.555.1")).toBeInTheDocument();
      expect(screen.queryByText(/→ 2\.555$/)).not.toBeInTheDocument();
    });

    it("shows a drift badge when running and desired genuinely differ", () => {
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a", { jenkinsVersion: "2.541.1", version: "2.555" })],
        isLoading: false,
        error: null,
      });
      renderPicker();
      expect(screen.getByText("2.541.1")).toBeInTheDocument();
      expect(screen.getByText(/→ 2.555/)).toBeInTheDocument();
    });

    it("falls back to desired version when the running version is unknown", () => {
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a", { jenkinsVersion: undefined, version: "2.555" })],
        isLoading: false,
        error: null,
      });
      renderPicker();
      expect(screen.getByText("2.555")).toBeInTheDocument();
    });
  });

  describe("Phase filter", () => {
    it("renders phase filter chips with counts", () => {
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { phase: "Running" }),
          ctrlFixture("ctrl-b", { phase: "Provisioning" }),
          ctrlFixture("ctrl-c", { phase: "Provisioning" }),
          ctrlFixture("ctrl-d", { phase: "Failed" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      expect(screen.getByRole("button", { name: "All 4" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /Running 1/ })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /Provisioning 2/ })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /Failed 1/ })).toBeInTheDocument();
    });

    it("filters controllers by phase when chip is clicked", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-running", { phase: "Running" }),
          ctrlFixture("ctrl-failed", { phase: "Failed" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      await user.click(screen.getByRole("button", { name: /Failed/ }));

      expect(screen.getByText("ctrl-failed")).toBeInTheDocument();
      expect(screen.queryByText("ctrl-running")).not.toBeInTheDocument();
    });

    it('shows "No controllers match the current filters" when filter yields no results', async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-running", { phase: "Running" })],
        isLoading: false,
        error: null,
      });
      renderPicker();

      await user.click(screen.getByText("Failed"));

      expect(screen.getByText(/No controllers match the current filters/)).toBeInTheDocument();
    });
  });

  describe("Name filter", () => {
    it("filters controllers by name when typing in search box", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("production-build"),
          ctrlFixture("staging-build"),
          ctrlFixture("admin-tools"),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      await user.type(screen.getByPlaceholderText("Filter by name..."), "build");

      expect(screen.getByText("production-build")).toBeInTheDocument();
      expect(screen.getByText("staging-build")).toBeInTheDocument();
      expect(screen.queryByText("admin-tools")).not.toBeInTheDocument();
    });
  });

  describe("Namespace filter", () => {
    it("renders a namespace select with All namespaces and per-namespace options", () => {
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { namespace: "ns1" }),
          ctrlFixture("ctrl-b", { namespace: "ns1" }),
          ctrlFixture("ctrl-c", { namespace: "ns2" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const select = screen.getByRole("combobox", { name: "Namespace" });
      expect(select).toBeInTheDocument();
      expect(select).toHaveValue("");
      expect(screen.getByText(/All namespaces/)).toBeInTheDocument();
      expect(screen.getByText(/ns1 \(2\)/)).toBeInTheDocument();
      expect(screen.getByText(/ns2 \(1\)/)).toBeInTheDocument();
    });

    it("filters controllers by namespace when a namespace is selected", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { namespace: "ns1" }),
          ctrlFixture("ctrl-b", { namespace: "ns2" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const select = screen.getByRole("combobox", { name: "Namespace" });
      await user.selectOptions(select, "ns1");

      expect(screen.getByText("ctrl-a")).toBeInTheDocument();
      expect(screen.queryByText("ctrl-b")).not.toBeInTheDocument();
    });

    it("gives all three filter dropdowns the same styling and no stray caption", () => {
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a", { namespace: "ns1", phase: "Running" })],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const ns = screen.getByRole("combobox", { name: "Namespace" });
      const cluster = screen.getByRole("combobox", { name: "Cluster" });
      const group = screen.getByRole("combobox", { name: "Group by" });

      // The namespace select used to be styled by a separate .filterField rule
      // (34px tall, different radius, unset font-size) and sat lower than its
      // siblings because its captioned wrapper was taller.
      const shared = cluster.className.trim();
      expect(shared).not.toBe("");
      expect(ns.className).toContain(shared);
      expect(group.className).toContain(shared);

      // Caption dropped: the option text is self-describing, like its siblings.
      // aria-label carries the accessible name (asserted by the queries above).
      expect(ns.closest("label")).toBeNull();
    });

    it("ANDs the namespace filter with the phase filter", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { namespace: "ns1", phase: "Running" }),
          ctrlFixture("ctrl-b", { namespace: "ns1", phase: "Failed" }),
          ctrlFixture("ctrl-c", { namespace: "ns2", phase: "Running" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const select = screen.getByRole("combobox", { name: "Namespace" });
      await user.selectOptions(select, "ns1");
      await user.click(screen.getByRole("button", { name: /Failed/ }));

      expect(screen.getByText("ctrl-b")).toBeInTheDocument();
      expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
      expect(screen.queryByText("ctrl-c")).not.toBeInTheDocument();
    });
  });

  describe("Group by", () => {
    it("renders a Group by select and shows section headers when set to namespace", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { namespace: "ns2" }),
          ctrlFixture("ctrl-b", { namespace: "ns1" }),
          ctrlFixture("ctrl-c", { namespace: "ns2" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const select = screen.getByLabelText("Group by");
      await user.selectOptions(select, "namespace");

      expect(screen.getAllByText("ns1 (1)").length).toBeGreaterThanOrEqual(1);
      expect(screen.getAllByText("ns2 (2)").length).toBeGreaterThanOrEqual(1);
      expect(screen.getByText("ctrl-a")).toBeInTheDocument();
      expect(screen.getByText("ctrl-b")).toBeInTheDocument();
      expect(screen.getByText("ctrl-c")).toBeInTheDocument();
    });

    it("skips empty groups when cross-filtered", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { namespace: "ns1", phase: "Running" }),
          ctrlFixture("ctrl-b", { namespace: "ns2", phase: "Failed" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      await user.click(screen.getByRole("button", { name: /Running/ }));
      const select = screen.getByLabelText("Group by");
      await user.selectOptions(select, "namespace");

      expect(screen.getAllByText("ns1 (1)").length).toBeGreaterThanOrEqual(1);
      expect(screen.getAllByText(/ns2/).length).toBe(1);
    });
  });

  describe("Selection", () => {
    it("calls onSelectionChange with the cluster/ns/name key when a row checkbox is checked", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a"), ctrlFixture("ctrl-b")],
        isLoading: false,
        error: null,
      });
      renderPicker(onSelectionChange);

      const checkboxes = screen.getAllByRole("checkbox");
      await user.click(checkboxes[1]);

      expect(onSelectionChange).toHaveBeenCalledWith(["core/default/ctrl-a"]);
    });

    it("select-all toggles every filtered row", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a"), ctrlFixture("ctrl-b")],
        isLoading: false,
        error: null,
      });
      renderPicker(onSelectionChange);

      const header = screen.getAllByRole("checkbox")[0];
      await user.click(header);

      expect(onSelectionChange).toHaveBeenCalledWith(["core/default/ctrl-a", "core/default/ctrl-b"]);
    });

    it("select-all is unchecked (and preserves out-of-filter selections) when the count merely coincides", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { namespace: "team-a" }),
          ctrlFixture("ctrl-b", { namespace: "team-b" }),
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(
        <BroodControllerPicker selected={["core/team-a/ctrl-a"]} onSelectionChange={onSelectionChange} />,
      );

      const select = screen.getByRole("combobox", { name: "Namespace" });
      await user.selectOptions(select, "team-b");

      const header = screen.getAllByRole("checkbox")[0];
      expect(header).not.toBeChecked();

      await user.click(header);

      expect(onSelectionChange).toHaveBeenCalledWith(["core/team-a/ctrl-a", "core/team-b/ctrl-b"]);
    });

    it("compact mode: clicking a row with nothing selected toggles selection instead of navigating away", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
      });
      renderPicker(onSelectionChange, true);

      await user.click(screen.getByText("ctrl-a"));

      expect(onSelectionChange).toHaveBeenCalledWith(["core/default/ctrl-a"]);
      expect(mockNavigate).not.toHaveBeenCalled();
    });

    it("compact mode: clicking a row does nothing when the user lacks manage permission", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseAuth.mockReturnValue({
        permissions: {
          global: {
            controllers: { create: false, delete: false, get: true, list: true, update: false, manage: false },
          },
          scopes: [],
        },
      });
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
      });
      renderPicker(onSelectionChange, true);

      await user.click(screen.getByText("ctrl-a"));

      expect(onSelectionChange).not.toHaveBeenCalled();
      expect(mockNavigate).not.toHaveBeenCalled();
    });
  });

  describe("Grid column alignment without manage permission", () => {
    it("keeps the same number of grid cells in the header and rows whether or not the checkbox column renders", () => {
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
      });

      mockUseAuth.mockReturnValue(managePermissions);
      const { container: withManage, unmount } = renderPicker();
      const rowsWithManage = withManage.querySelectorAll('[class*="ctrlRow"]');
      const cellCountsWithManage = Array.from(rowsWithManage).map((r) => r.children.length);
      unmount();

      mockUseAuth.mockReturnValue({
        permissions: {
          global: {
            controllers: { create: false, delete: false, get: true, list: true, update: false, manage: false },
          },
          scopes: [],
        },
      });
      const { container: withoutManage } = renderPicker();
      const rowsWithoutManage = withoutManage.querySelectorAll('[class*="ctrlRow"]');
      const cellCountsWithoutManage = Array.from(rowsWithoutManage).map((r) => r.children.length);

      expect(rowsWithManage.length).toBeGreaterThan(0);
      expect(cellCountsWithoutManage).toEqual(cellCountsWithManage);
    });

    it("clicking the empty checkbox cell still navigates to the controller when the user lacks manage permission", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
      });
      mockUseAuth.mockReturnValue({
        permissions: {
          global: {
            controllers: { create: false, delete: false, get: true, list: true, update: false, manage: false },
          },
          scopes: [],
        },
      });

      const { container } = renderPicker();
      const row = container.querySelector('[class*="ctrlRow"]:not([class*="head"])')!;
      const checkboxCell = row.firstElementChild as HTMLElement;

      await user.click(checkboxCell);

      expect(mockNavigate).toHaveBeenCalledWith("/controllers/core/default/ctrl-a");
    });
  });

  describe("Cluster filter (#309 review)", () => {
    it("compact mode keeps the cluster filter local and does not touch the host page's URL", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a", { cluster: "core" }), ctrlFixture("ctrl-b", { cluster: "hive" })],
        isLoading: false,
        error: null,
      });

      function LocationProbe() {
        const location = useLocation();
        return <div data-testid="location">{location.pathname}{location.search}</div>;
      }

      renderWithProviders(
        <>
          <LocationProbe />
          <BroodControllerPicker selected={[]} onSelectionChange={vi.fn()} compact />
        </>,
        { route: "/brood-operations?foo=bar" },
      );

      expect(screen.getByTestId("location")).toHaveTextContent("/brood-operations?foo=bar");

      await user.selectOptions(screen.getByLabelText("Cluster"), "hive");

      expect(screen.getByLabelText("Cluster")).toHaveValue("hive");
      expect(screen.getByTestId("location")).toHaveTextContent("/brood-operations?foo=bar");
    });

    it("non-compact mode syncs the cluster filter to the URL while preserving other query params", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a", { cluster: "core" }), ctrlFixture("ctrl-b", { cluster: "hive" })],
        isLoading: false,
        error: null,
      });

      function LocationProbe() {
        const location = useLocation();
        return <div data-testid="location">{location.pathname}{location.search}</div>;
      }

      renderWithProviders(
        <>
          <LocationProbe />
          <BroodControllerPicker selected={[]} onSelectionChange={vi.fn()} />
        </>,
        { route: "/controllers?foo=bar" },
      );

      await user.selectOptions(screen.getByLabelText("Cluster"), "hive");
      expect(screen.getByTestId("location")).toHaveTextContent("/controllers?foo=bar&cluster=hive");

      await user.selectOptions(screen.getByLabelText("Cluster"), "");
      expect(screen.getByTestId("location")).toHaveTextContent("/controllers?foo=bar");
    });
  });

  describe("Keyboard accessibility (#309 review)", () => {
    it("activates a row via Enter like a click", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
      });

      renderPicker(onSelectionChange, true);
      const row = screen.getByRole("button", { name: /Controller ctrl-a/ });
      row.focus();
      await user.keyboard("{Enter}");

      expect(onSelectionChange).toHaveBeenCalledWith(["core/default/ctrl-a"]);
    });

    it("activates a row via Space like a click", async () => {
      const user = userEvent.setup();
      const onSelectionChange = vi.fn();
      mockUseControllers.mockReturnValue({
        data: [ctrlFixture("ctrl-a")],
        isLoading: false,
        error: null,
      });

      renderPicker(onSelectionChange, true);
      const row = screen.getByRole("button", { name: /Controller ctrl-a/ });
      row.focus();
      await user.keyboard(" ");

      expect(onSelectionChange).toHaveBeenCalledWith(["core/default/ctrl-a"]);
    });
  });

  describe("Deterministic default sort (#438)", () => {
    it("renders rows sorted by name ascending regardless of API order", () => {
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("zebra"),
          ctrlFixture("apple"),
          ctrlFixture("mango"),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const names = screen.getAllByRole("button", { name: /^Controller / }).map((row) =>
        row.getAttribute("aria-label"),
      );
      expect(names).toEqual([
        "Controller apple (default/core)",
        "Controller mango (default/core)",
        "Controller zebra (default/core)",
      ]);
    });
  });

  describe("Column sorting (#438)", () => {
    it("sorts by cluster when the Cluster header is clicked, and marks aria-sort", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { cluster: "zzz" }),
          ctrlFixture("ctrl-b", { cluster: "aaa" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const nameHeader = screen.getByRole("columnheader", { name: "Controller" });
      expect(nameHeader).toHaveAttribute("aria-sort", "ascending");

      await user.click(screen.getByRole("button", { name: "Cluster" }));

      const clusterHeader = screen.getByRole("columnheader", { name: "Cluster" });
      expect(clusterHeader).toHaveAttribute("aria-sort", "ascending");

      const names = screen.getAllByRole("button", { name: /^Controller / }).map((row) =>
        row.getAttribute("aria-label"),
      );
      expect(names).toEqual([
        "Controller ctrl-b (default/aaa)",
        "Controller ctrl-a (default/zzz)",
      ]);

      await user.click(screen.getByRole("button", { name: "Cluster" }));
      expect(screen.getByRole("columnheader", { name: "Cluster" })).toHaveAttribute(
        "aria-sort",
        "descending",
      );
      const namesDesc = screen.getAllByRole("button", { name: /^Controller / }).map((row) =>
        row.getAttribute("aria-label"),
      );
      expect(namesDesc).toEqual([
        "Controller ctrl-a (default/zzz)",
        "Controller ctrl-b (default/aaa)",
      ]);
    });
  });

  describe("Mite column accessibility (#438)", () => {
    it("exposes connected/disconnected state to assistive tech and sighted users", () => {
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { miteConnected: true }),
          ctrlFixture("ctrl-b", { miteConnected: false }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      expect(screen.getByRole("img", { name: "mite connected" })).toBeInTheDocument();
      expect(screen.getByRole("img", { name: "mite disconnected" })).toBeInTheDocument();
      const rowA = screen.getByRole("button", { name: /Controller ctrl-a/ });
      const rowB = screen.getByRole("button", { name: /Controller ctrl-b/ });
      expect(within(rowA).getByText("Connected")).toBeInTheDocument();
      expect(within(rowB).getByText("Disconnected")).toBeInTheDocument();
    });
  });

  describe("Hibernated filter chip (#438)", () => {
    it("adds a Hibernated chip that filters to hibernated controllers", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { phase: "Running" }),
          ctrlFixture("ctrl-b", { phase: "Hibernated" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      const chip = screen.getByRole("button", { name: /Hibernated/ });
      expect(chip).toBeInTheDocument();

      await user.click(chip);

      expect(screen.getByText("ctrl-b")).toBeInTheDocument();
      expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
    });

    it("offers a Needs attention chip that filters to controllers with attention", async () => {
      const user = userEvent.setup();
      mockUseControllers.mockReturnValue({
        data: [
          ctrlFixture("ctrl-a", { phase: "Provisioning", attention: { kind: "reconcileBlocked", message: "m" } }),
          ctrlFixture("ctrl-b", { phase: "Provisioning" }),
        ],
        isLoading: false,
        error: null,
      });
      renderPicker();

      await user.click(screen.getByRole("button", { name: /Needs attention/ }));

      expect(screen.getByText("ctrl-a")).toBeInTheDocument();
      expect(screen.queryByText("ctrl-b")).not.toBeInTheDocument();
      expect(screen.getByText("Blocked")).toBeInTheDocument();
    });
  });
});
