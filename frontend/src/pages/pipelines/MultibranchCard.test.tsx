import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { JobBranchList } from "../../types";
import { MultibranchCard } from "./MultibranchCard";

const mockListJobBranches = vi.fn();

vi.mock("../../api/client", () => ({
  listJobBranches: (...args: unknown[]) => mockListJobBranches(...args),
}));

beforeEach(() => {
  mockListJobBranches.mockReset();
});

function renderCard() {
  const client = createTestQueryClient();
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <MultibranchCard team="alpha" jobPath="alpha/app" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const inventory: JobBranchList = {
  jobPath: "alpha/app",
  observedAt: "2026-10-12T10:00:00Z",
  truncated: false,
  branches: [
    {
      name: "main",
      jobPath: "alpha/app/main",
      orphaned: false,
      revision: "abcdef1234",
      lastRun: { runId: "run-main", status: "COMPLETED", result: "SUCCESS" },
    },
  ],
  tags: [],
  changeRequests: [
    {
      name: "PR-7",
      jobPath: "alpha/app/PR-7",
      orphaned: true,
      revision: "0123456789",
      changeRequest: { id: "7", title: "Fix the build" },
    },
  ],
};

describe("MultibranchCard", () => {
  it("lists branches and pull requests with links to their job pages", async () => {
    mockListJobBranches.mockResolvedValue(inventory);
    renderCard();

    const branch = await screen.findByRole("link", { name: "main" });
    expect(branch.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Fapp%2Fmain");
    expect(screen.getByRole("link", { name: "#7 Fix the build" })).toBeInTheDocument();
    expect(screen.getByText("orphaned")).toBeInTheDocument();
    expect(screen.getByText("abcdef1")).toBeInTheDocument();
  });

  it("links the latest run result to its build page", async () => {
    mockListJobBranches.mockResolvedValue(inventory);
    renderCard();

    const result = await screen.findByRole("link", { name: "SUCCESS" });
    expect(result.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Fapp%2Fmain/run-main");
  });

  it("states when the cell has not exported the job yet", async () => {
    mockListJobBranches.mockResolvedValue({
      jobPath: "alpha/app",
      truncated: false,
      branches: [],
      changeRequests: [],
      tags: [],
    });
    renderCard();

    await waitFor(() => {
      expect(screen.getByText("The cell has not exported this job yet.")).toBeInTheDocument();
    });
    expect(screen.getByText("No branches discovered.")).toBeInTheDocument();
    expect(screen.getByText("No pull requests discovered.")).toBeInTheDocument();
  });

  it("shows the inventory age and the staleness note", async () => {
    mockListJobBranches.mockResolvedValue(inventory);
    renderCard();

    expect(await screen.findByText(/Inventory as of/)).toBeInTheDocument();
    expect(screen.getByText(/can be up to ten minutes old/)).toBeInTheDocument();
  });
});
