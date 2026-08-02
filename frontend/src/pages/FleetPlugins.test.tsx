import { describe, it, expect, beforeAll, afterEach, afterAll } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import FleetPlugins from "../pages/FleetPlugins";
import { renderWithProviders } from "../test/render-utils";
import { createHandlers, incompleteFleetPluginsRollup, rollupWithMissingControllers, rollupWithDetailStale, rollupWithUnknownClass, drilldownWithUnknownClass, drilldownWithBootstrapApproximate } from "../test/handlers";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";

const BASE = "/api/v1";
const server = setupServer(...createHandlers());

beforeAll(() => server.listen({ onUnhandledRequest: "bypass" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

describe("FleetPlugins", () => {
  it("renders page title and description", async () => {
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("Plugins")).toBeInTheDocument();
      expect(screen.getByText("Fleet-wide installed plugin inventory")).toBeInTheDocument();
    });
  });

  it("renders the filter bar with plugin name and version range inputs", () => {
    renderWithProviders(<FleetPlugins />);
    expect(screen.getByPlaceholderText("Plugin name…")).toBeInTheDocument();
    expect(screen.getByPlaceholderText(/Version range/)).toBeInTheDocument();
  });

  it("renders the coverage notice when coverage is incomplete", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(incompleteFleetPluginsRollup()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText(/This release covers the local cluster only/)).toBeInTheDocument();
    });
  });

  it("renders coverage notice with role=status", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(incompleteFleetPluginsRollup()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByRole("status", { name: "Coverage notice" })).toBeInTheDocument();
    });
  });

  it("renders the coverage notice with controllersMissing", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithMissingControllers()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText(/Some controllers have no observed inventory/)).toBeInTheDocument();
      expect(screen.getByText(/2 controllers absent/)).toBeInTheDocument();
    });
  });

  it("renders coverage notice with controllersDetailStale count", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText(/with detail-stale classification/)).toBeInTheDocument();
    });
  });

  it("renders the rollup table with items", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("git-client")).toBeInTheDocument();
    });
  });

  it("renders rollup classes breakdown", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("declared")).toBeInTheDocument();
    });
  });

  it("clicking a rollup row opens the drilldown panel", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
      http.get(`${BASE}/fleet/plugins/:name`, () =>
        HttpResponse.json(drilldownWithBootstrapApproximate()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("git-client")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText("git-client"));
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "git-client" })).toBeInTheDocument();
    });
  });

  it("renders an unknown class label verbatim in rollup", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithUnknownClass()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("future-label-xyz")).toBeInTheDocument();
    });
  });

  it("renders an unknown class label verbatim in drilldown row", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithUnknownClass()),
      ),
      http.get(`${BASE}/fleet/plugins/:name`, () =>
        HttpResponse.json(drilldownWithUnknownClass()),
      ),
    );
    renderWithProviders(<FleetPlugins />, { route: "/plugins?plugin=mystery-plugin" });
    await waitFor(() => {
      const classes = screen.getAllByText("future-label-xyz");
      expect(classes.length).toBeGreaterThanOrEqual(1);
    });
  });

  it("renders bootstrapApproximate independently of degraded", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
      http.get(`${BASE}/fleet/plugins/:name`, () =>
        HttpResponse.json(drilldownWithBootstrapApproximate()),
      ),
    );
    renderWithProviders(<FleetPlugins />, { route: "/plugins?plugin=git-client" });
    await waitFor(() => {
      expect(screen.getByText("approx")).toBeInTheDocument();
    });
  });

  it("renders the detailPath as a link on drilldown rows", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
      http.get(`${BASE}/fleet/plugins/:name`, () =>
        HttpResponse.json(drilldownWithBootstrapApproximate()),
      ),
    );
    renderWithProviders(<FleetPlugins />, { route: "/plugins?plugin=git-client" });
    await waitFor(() => {
      const link = screen.getByRole("link", { name: "ctrl-a" });
      expect(link).toHaveAttribute("href", "/api/v1/clusters/core/controllers/ns/ctrl-a/plugins");
    });
  });

  it("shows the exact empty-state copy when results are empty and coverage is incomplete", async () => {
    const incompleteEmpty = {
      items: [] as never[],
      coverage: {
        complete: false,
        controllersTotal: 0,
        controllersReporting: 0,
        controllersStale: 0,
        controllersDegraded: 0,
        controllersTruncated: 0,
        controllersDetailStale: 0,
        controllersMissing: [] as never[],
        clustersNotCovered: 1,
      },
      clusters: [
        { name: "core", ok: true },
        { name: "remote", ok: false, error: "v1 covers the local cluster only (R22)" },
      ],
    };
    server.use(
      http.get(`${BASE}/fleet/plugins`, () => HttpResponse.json(incompleteEmpty)),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("No matches among the controllers we could see.")).toBeInTheDocument();
    });
  });

  it("coverage notice still renders above the empty state when incomplete", async () => {
    const incompleteEmpty = {
      items: [] as never[],
      coverage: {
        complete: false,
        controllersTotal: 0,
        controllersReporting: 0,
        controllersStale: 0,
        controllersDegraded: 0,
        controllersTruncated: 0,
        controllersDetailStale: 0,
        controllersMissing: [] as never[],
        clustersNotCovered: 1,
      },
      clusters: [
        { name: "core", ok: true },
        { name: "remote", ok: false, error: "v1 covers the local cluster only (R22)" },
      ],
    };
    server.use(
      http.get(`${BASE}/fleet/plugins`, () => HttpResponse.json(incompleteEmpty)),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByRole("status", { name: "Coverage notice" })).toBeInTheDocument();
      expect(screen.getByText("No matches among the controllers we could see.")).toBeInTheDocument();
    });
  });

  it("renders a 502 error state, never an empty fleet", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        new HttpResponse(
          JSON.stringify({ error: "fleet plugin inventory is not available" }),
          { status: 502, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("Plugin inventory is not available.")).toBeInTheDocument();
      expect(screen.getByText("The backend dependency is not wired.")).toBeInTheDocument();
    });
  });

  it("filters by plugin name via URL search params", async () => {
    renderWithProviders(<FleetPlugins />, { route: "/plugins?q=git" });
    await waitFor(() => {
      const input = screen.getByPlaceholderText("Plugin name…") as HTMLInputElement;
      expect(input.value).toBe("git");
    });
  });

  it("filters by affected via URL search params", async () => {
    renderWithProviders(<FleetPlugins />, { route: "/plugins?affected=<=4.0.0" });
    await waitFor(() => {
      const input = screen.getByPlaceholderText(/Version range/) as HTMLInputElement;
      expect(input.value).toBe("<=4.0.0");
    });
  });

  it("selecting a plugin sets the URL search param", async () => {
    server.use(
      http.get(`${BASE}/fleet/plugins`, () =>
        HttpResponse.json(rollupWithDetailStale()),
      ),
      http.get(`${BASE}/fleet/plugins/:name`, () =>
        HttpResponse.json(drilldownWithBootstrapApproximate()),
      ),
    );
    renderWithProviders(<FleetPlugins />);
    await waitFor(() => {
      expect(screen.getByText("git-client")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText("git-client"));
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "git-client" })).toBeInTheDocument();
    });
  });
});
