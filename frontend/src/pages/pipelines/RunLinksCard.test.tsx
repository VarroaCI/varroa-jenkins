import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { RunLink, RunLinkPage } from "../../types";
import { RunLinksCard } from "./RunLinksCard";

const mockListRunDownstream = vi.fn();
vi.mock("../../api/client", () => ({
  listRunDownstream: (...args: unknown[]) => mockListRunDownstream(...args),
}));

beforeEach(() => {
  mockListRunDownstream.mockReset();
});

function renderCard(props: { upstream?: RunLink; downstream?: RunLinkPage }) {
  const client = createTestQueryClient();
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <RunLinksCard runId="run-self" {...props} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RunLinksCard", () => {
  it("links the triggering run to its build page", () => {
    renderCard({
      upstream: { tenant: "alpha", jobPath: "alpha/release", runId: "run-up", buildNumber: 4 },
    });
    const link = screen.getByRole("link", { name: "alpha/release #4" });
    expect(link.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Frelease/run-up");
  });

  it("shows a tenant alone with no link when the reader may not read the run", () => {
    renderCard({ upstream: { tenant: "beta" } });
    expect(screen.getByText("beta")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("shows the job and number as text when no run id matched", () => {
    renderCard({ upstream: { tenant: "alpha", jobPath: "alpha/release", buildNumber: 4 } });
    expect(screen.getByText("alpha/release #4")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("lists each downstream run as a link with its status", () => {
    renderCard({
      downstream: {
        items: [
          { tenant: "beta", jobPath: "beta/rebuild", runId: "run-c1", buildNumber: 2, status: "COMPLETED", result: "SUCCESS" },
          { tenant: "gamma" },
        ],
      },
    });
    const link = screen.getByRole("link", { name: "beta/rebuild #2" });
    expect(link.getAttribute("href")).toBe("/pipelines/beta/beta%2Frebuild/run-c1");
    expect(screen.getByText(/COMPLETED · SUCCESS/)).toBeInTheDocument();
    expect(screen.getByText("gamma")).toBeInTheDocument();
  });

  it("reads the next downstream page on Show more", async () => {
    mockListRunDownstream.mockResolvedValue({
      items: [{ tenant: "delta", jobPath: "delta/deploy", runId: "run-d1", buildNumber: 7, status: "RUNNING" }],
    });
    renderCard({
      downstream: {
        items: [{ tenant: "beta", jobPath: "beta/rebuild", runId: "run-c1", buildNumber: 2 }],
        nextCursor: "cursor-1",
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Show more" }));
    await waitFor(() => expect(screen.getByRole("link", { name: "delta/deploy #7" })).toBeInTheDocument());
    expect(mockListRunDownstream).toHaveBeenCalledWith("run-self", "cursor-1");
    expect(screen.queryByRole("button", { name: "Show more" })).not.toBeInTheDocument();
  });

  it("shows a refetched status for the same downstream run", () => {
    const client = createTestQueryClient();
    const tree = (downstream: RunLinkPage) => (
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <RunLinksCard runId="run-self" downstream={downstream} />
        </MemoryRouter>
      </QueryClientProvider>
    );
    const { rerender } = render(
      tree({ items: [{ tenant: "beta", jobPath: "beta/rebuild", runId: "run-c1", buildNumber: 2, status: "RUNNING" }] }),
    );
    expect(screen.getByText(/RUNNING/)).toBeInTheDocument();
    rerender(
      tree({ items: [{ tenant: "beta", jobPath: "beta/rebuild", runId: "run-c1", buildNumber: 2, status: "COMPLETED", result: "SUCCESS" }] }),
    );
    expect(screen.getByText(/COMPLETED · SUCCESS/)).toBeInTheDocument();
    expect(screen.queryByText(/RUNNING/)).not.toBeInTheDocument();
  });

  it("renders nothing when neither link exists", () => {
    const { container } = renderCard({ downstream: { items: [] } });
    expect(container).toBeEmptyDOMElement();
  });
});
