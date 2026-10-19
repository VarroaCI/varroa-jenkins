import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { JobBranchList } from "../../types";
import { MultibranchCard } from "./MultibranchCard";

const mockListJobBranches = vi.fn();
const mockScanMultibranch = vi.fn();

vi.mock("../../api/client", () => ({
  listJobBranches: (...args: unknown[]) => mockListJobBranches(...args),
  scanMultibranch: (...args: unknown[]) => mockScanMultibranch(...args),
}));

beforeEach(() => {
  mockListJobBranches.mockReset();
  mockScanMultibranch.mockReset();
});

function renderCard(canScan = false) {
  const client = createTestQueryClient();
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <MultibranchCard team="alpha" jobPath="alpha/app" canScan={canScan} />
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
  repositories: [],
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

  it("lists the repositories of an organization folder with links and no run column", async () => {
    mockListJobBranches.mockResolvedValue({
      jobPath: "alpha/repos",
      truncated: false,
      branches: [],
      changeRequests: [],
      tags: [],
      repositories: [
        { name: "projgen", jobPath: "alpha/repos/projgen", orphaned: false, revision: "" },
      ],
    });
    renderCard();

    const repository = await screen.findByRole("link", { name: "projgen" });
    expect(repository.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Frepos%2Fprojgen");
    expect(screen.getByText("Repositories")).toBeInTheDocument();
    expect(screen.queryByRole("columnheader", { name: "Latest run" })).toBeNull();
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
      repositories: [],
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

  it("hides the Scan now button from a caller who may not write the job", async () => {
    mockListJobBranches.mockResolvedValue(inventory);
    renderCard(false);

    await screen.findByRole("link", { name: "main" });
    expect(screen.queryByRole("button", { name: "Scan now" })).not.toBeInTheDocument();
  });

  it("requests a scan and shows the counts and names of its result", async () => {
    mockListJobBranches
      .mockResolvedValueOnce(inventory)
      .mockResolvedValue({
        ...inventory,
        lastScan: {
          scanId: "scan-1",
          state: "completed",
          result: "SUCCESS",
          newHeads: ["feature"],
          changedHeads: ["main"],
          orphanedHeads: [],
        },
      });
    mockScanMultibranch.mockResolvedValue({ scanId: "scan-1", state: "requested" });
    renderCard(true);

    fireEvent.click(await screen.findByRole("button", { name: "Scan now" }));

    await waitFor(() => expect(mockScanMultibranch).toHaveBeenCalledWith("alpha", "alpha/app"));
    expect(await screen.findByText("1 new, 1 changed, 0 orphaned")).toBeInTheDocument();
    expect(screen.getByText("feature")).toBeInTheDocument();
  });

  it("shows the reason a scan failed", async () => {
    mockListJobBranches.mockResolvedValue({
      ...inventory,
      lastScan: {
        scanId: "scan-2",
        state: "failed",
        result: "FAILURE",
        message: "the folder computation ended with FAILURE",
        newHeads: [],
        changedHeads: [],
        orphanedHeads: [],
      },
    });
    renderCard(true);

    expect(await screen.findByText(/The scan failed\./)).toBeInTheDocument();
    expect(screen.getByText(/the folder computation ended with FAILURE/)).toBeInTheDocument();
  });

  it("says when the scan request itself fails", async () => {
    mockListJobBranches.mockResolvedValue(inventory);
    mockScanMultibranch.mockRejectedValue(new Error("forbidden"));
    renderCard(true);

    fireEvent.click(await screen.findByRole("button", { name: "Scan now" }));

    expect(await screen.findByText("The scan could not be requested.")).toBeInTheDocument();
    await waitFor(() => expect(mockScanMultibranch).toHaveBeenCalledTimes(1));
  });
});
