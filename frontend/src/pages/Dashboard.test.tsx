import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import Dashboard from "./Dashboard";
import { renderWithProviders } from "../test/render-utils";

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

vi.mock("../hooks/useApi", () => ({
  bffFetch: vi.fn().mockResolvedValue({items: []}),
}));

import * as useApi from "../hooks/useApi";

describe("Dashboard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe("Loading state", () => {
    it("shows skeleton metric cards when loading", () => {
      mockUseControllers.mockReturnValue({
        data: null,
        isLoading: true,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("Brood overview")).toBeInTheDocument();
      expect(screen.getByText("Loading...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner when loading fails", () => {
      mockUseControllers.mockReturnValue({
        data: null,
        isLoading: false,
        error: { message: "Connection refused" },
      });
      renderWithProviders(<Dashboard />);

      expect(
        screen.getByText(/Failed to load brood data: Connection refused/),
      ).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it('shows "No controllers deployed yet" when there are no controllers', () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(
        screen.getByText(/No controllers deployed yet/),
      ).toBeInTheDocument();
    });
  });

  describe("Happy path", () => {
    it("renders the page heading and description with total count", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("Brood overview")).toBeInTheDocument();
      expect(screen.getByText(/2 controllers across 1 namespace$/)).toBeInTheDocument();
    });

    it("counts (cluster, namespace) identities, not bare namespace names", () => {
      mockUseControllers.mockReturnValue({
        data: [
          // Same namespace name in two clusters must count as two.
          { name: "ctrl-a", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-b", cluster: "cluster-b", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-c", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText(/3 controllers across 2 namespaces/)).toBeInTheDocument();
    });

    it("uses singular nouns for a single controller in a single namespace", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText(/1 controller across 1 namespace$/)).toBeInTheDocument();
    });

    it("no longer advertises an unwired operator uptime", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.queryByText(/operator uptime/)).not.toBeInTheDocument();
      expect(screen.queryByText(/added this week/)).not.toBeInTheDocument();
    });

    it("renders metric cards: Total, Connected, Provisioning, Needs attention", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Provisioning", miteConnected: false, endpoint: "" },
          { name: "ctrl-c", cluster: "core", namespace: "default", phase: "Failed", miteConnected: false, endpoint: "" },
          { name: "ctrl-d", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("Total controllers")).toBeInTheDocument();
      expect(screen.getByText("Mites connected")).toBeInTheDocument();
      // "Provisioning" appears in both MetricCard label and StatusPill
      expect(screen.getAllByText("Provisioning").length).toBeGreaterThanOrEqual(1);
      expect(screen.getByText("Needs attention")).toBeInTheDocument();
      expect(screen.getByText("4")).toBeInTheDocument(); // total
      expect(screen.getByText("2 / 4")).toBeInTheDocument(); // connected / total
    });

    it("shows correct count for provisioning", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Provisioning", miteConnected: false, endpoint: "" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Provisioning", miteConnected: false, endpoint: "" },
          { name: "ctrl-c", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("2 in progress")).toBeInTheDocument();
    });

    it('shows "all clear" when there are no failed controllers', () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("all clear")).toBeInTheDocument();
    });

    it("renders the brood health card with controller names and links", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText(/Brood health/)).toBeInTheDocument();
      const link = screen.getByRole("link", { name: /ctrl-a/i });
      expect(link).toBeInTheDocument();
      expect(link).toHaveAttribute("href", "/controllers/core/default/ctrl-a");
    });

    it("renders the 'New controller' link", () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      const newCtrlLink = screen.getByText(/New controller/);
      expect(newCtrlLink).toBeInTheDocument();
      expect(newCtrlLink.closest("a")).toHaveAttribute("href", "/controllers/create");
    });
  });

  describe("Activity feed", () => {
    it("shows 'No activity yet' when events are empty", async () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      const { queryClient } = renderWithProviders(<Dashboard />);

      // Pre-set the activity query data
      queryClient.setQueryData(["activity"], []);

      await waitFor(() => {
        expect(
          screen.getByText(/No activity yet/),
        ).toBeInTheDocument();
      });
    });
  });

  describe("Update Center gaps chip", () => {
    afterEach(() => {
      vi.mocked(useApi.bffFetch).mockClear();
    });

    it("renders chip when enabled:true and gaps.length > 0", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({
            enabled: true,
            gaps: [{ plugin: "blueocean", version: "1.25.3", requiredBy: "profile-a" }],
            conditions: [], phase: "Ready", pluginCount: 1, storeBytes: 0, lastSyncTime: null, storageType: "", pullThroughEnabled: false,
          });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.getByText(/1 plugin missing from Update Center/)).toBeInTheDocument();
      });
      const link = screen.getByRole("link", { name: /1 plugin missing/ });
      expect(link).toHaveAttribute("href", "/administration/update-center");
    });

    it("does not render chip when enabled:false", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({ enabled: false, conditions: [], gaps: [], lastSyncTime: null, phase: "", pluginCount: 0, storeBytes: 0, storageType: "", pullThroughEnabled: false });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/missing from Update Center/)).not.toBeInTheDocument();
      });
    });

    it("does not render chip when enabled:true but gaps:[]", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({ enabled: true, gaps: [], conditions: [], phase: "Ready", pluginCount: 0, storeBytes: 0, lastSyncTime: null, storageType: "", pullThroughEnabled: false });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/missing from Update Center/)).not.toBeInTheDocument();
      });
    });

    it("does not render chip when fetch fails and no dashboard error", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.reject(new Error("network error"));
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/missing from Update Center/)).not.toBeInTheDocument();
      });
      // No dashboard-wide error banner should appear from the UC fetch failure
      expect(screen.queryByText(/Failed to load brood data/)).not.toBeInTheDocument();
    });
  });
});
