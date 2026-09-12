import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import TeamHome from "./TeamHome";

const mockListPipelines = vi.fn();
const mockListRunners = vi.fn();
vi.mock("../../api/client", () => ({
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
  listRunners: (...args: unknown[]) => mockListRunners(...args),
}));

function renderPage(route = "/pipelines/acme") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines/:team" element={<TeamHome />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("TeamHome", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockListRunners.mockResolvedValue([]);
  });

  it("shows a loading state before the response arrives", () => {
    mockListPipelines.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByText(/Loading pipelines/)).toBeInTheDocument();
  });

  it("shows the empty state for a team with no running builds and no pipelines", async () => {
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Nothing is running.")).toBeInTheDocument();
      expect(screen.getByText("No pipelines yet.")).toBeInTheDocument();
    });
  });

  it("renders a never-run job with an empty last build and no health glyph", async () => {
    mockListPipelines.mockResolvedValue({
      items: [{ jobPath: "payments/checkout-api/main", health: "unknown" }],
      runningNow: [],
      truncated: false,
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("payments/checkout-api/main")).toBeInTheDocument());
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /^#/ })).not.toBeInTheDocument();
  });

  it("names the health state for a screen reader", async () => {
    mockListPipelines.mockResolvedValue({
      items: [{ jobPath: "payments/checkout-api/main", health: "rain" }],
      runningNow: [],
      truncated: false,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("img", { name: /Rain/ })).toBeInTheDocument();
    });
  });

  it("states that the running-now view is bounded when the API reports truncation", async () => {
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [
        { runId: "r1", jobPath: "team/a/main", status: "RUNNING", buildNumber: 12, currentStage: "Build" },
      ],
      truncated: true,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/bounded/)).toBeInTheDocument();
    });
  });

  it("renders the forbidden state on a 403", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(403, "no"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Access denied")).toBeInTheDocument();
    });
  });

  it("renders the generic error state on a 500", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(500, "boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load page")).toBeInTheDocument();
    });
  });

  it("explains a disabled record store on a 503", async () => {
    mockListPipelines.mockRejectedValue(new ApiError(503, "off"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Pipeline data is off")).toBeInTheDocument();
    });
  });
});

describe("TeamHome — Runners section", () => {
  beforeEach(() => {
    mockListPipelines.mockReset();
    mockListRunners.mockReset();
    mockListPipelines.mockResolvedValue({ items: [], runningNow: [], truncated: false });
  });

  it("shows the recovery state for a team with no registered runners", async () => {
    mockListRunners.mockResolvedValue([]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("No runners registered for this team yet.")).toBeInTheDocument();
    });
  });

  it("renders a runner row with its launcher, labels, cell, and connection state", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "win-1",
        spec: { team: "acme", launcher: "ssh", labels: ["windows", "msbuild"], remoteFS: "C:\\J" },
        status: { nodeName: "acme-win-1", cell: "acme-cell-1", connected: true, lastSeen: new Date().toISOString() },
      },
    ]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("win-1")).toBeInTheDocument();
    });
    expect(screen.getByText("ssh")).toBeInTheDocument();
    expect(screen.getByText("windows, msbuild")).toBeInTheDocument();
    expect(screen.getByText("acme-cell-1")).toBeInTheDocument();
    expect(screen.getByText("Connected", { selector: "span" })).toBeInTheDocument();
  });

  it("renders an unconnected runner with dashes for its unset fields", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "agent-1",
        spec: { team: "acme", launcher: "inbound", remoteFS: "/home/jenkins" },
        status: { connected: false },
      },
    ]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("agent-1")).toBeInTheDocument();
    });
    expect(screen.getByText("inbound")).toBeInTheDocument();
    expect(screen.getByText("Not connected")).toBeInTheDocument();
  });

  it("never renders status.token, even when the API response carries one", async () => {
    mockListRunners.mockResolvedValue([
      {
        name: "agent-1",
        spec: { team: "acme", launcher: "inbound", remoteFS: "/home/jenkins" },
        status: { connected: true, token: "do-not-render-me" },
      },
    ]);
    const { container } = renderPage();
    await waitFor(() => {
      expect(screen.getByText("agent-1")).toBeInTheDocument();
    });
    expect(container.textContent).not.toContain("do-not-render-me");
  });

  it("shows a no-runners-loaded message when the runners request fails", async () => {
    mockListRunners.mockRejectedValue(new Error("boom"));
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Unable to load runners.")).toBeInTheDocument();
    });
  });
});
