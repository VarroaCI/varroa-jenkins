import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { getRun } from "../../api/client";
import type { RunDetail } from "../../types";
import { RunMetadata } from "./RunMetadata";

vi.mock("../../api/client", () => ({
  getRun: vi.fn(),
}));

const baseRun: RunDetail = {
  runId: "r1",
  status: "COMPLETED",
  result: "SUCCESS",
  buildNumber: 42,
  jobPath: "svc/main",
  branch: "main",
  scmRevision: "abcdef1234567890",
  startedAt: "2026-10-11T10:00:12Z",
  durationMs: 61000,
} as RunDetail;

function renderMeta(run: RunDetail) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <RunMetadata run={run} team="team-a" job="svc/main" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RunMetadata", () => {
  beforeEach(() => {
    vi.mocked(getRun).mockReset();
  });

  it("links the branch to the job page filtered to that branch", () => {
    renderMeta(baseRun);
    expect(screen.getByRole("link", { name: "main" })).toHaveAttribute(
      "href",
      "/pipelines/team-a/svc%2Fmain?branch=main",
    );
  });

  it("names the trigger source in the actor tooltip", () => {
    renderMeta({ ...baseRun, actor: "alice", triggerSource: "cron" });
    expect(screen.getByText("alice")).toHaveAttribute("title", "Triggered by a schedule");
  });

  it("shows the queue wait above five seconds only", () => {
    renderMeta({ ...baseRun, queuedAt: "2026-10-11T10:00:00Z" });
    expect(screen.getByText(/Waited .* in queue/)).toBeInTheDocument();
  });

  it("hides the queue wait at or below five seconds", () => {
    renderMeta({ ...baseRun, queuedAt: "2026-10-11T10:00:08Z" });
    expect(screen.queryByText(/in queue/)).toBeNull();
  });

  it("lists parameters sorted, and masks a secret value", () => {
    const { container } = renderMeta({
      ...baseRun,
      params: { zeta: "9", alpha: "<masked>" },
    });
    const row = screen.getByText("Parameters").parentElement!;
    expect(within(row).getByText("alpha=••••")).toBeInTheDocument();
    expect(within(row).getByText("zeta=9")).toBeInTheDocument();
    const chips = Array.from(row.querySelectorAll("span")).map((s) => s.textContent);
    expect(chips.indexOf("alpha=••••")).toBeLessThan(chips.indexOf("zeta=9"));
    expect(container).toBeTruthy();
  });

  it("hides the parameters row without params", () => {
    renderMeta(baseRun);
    expect(screen.queryByText("Parameters")).toBeNull();
  });

  it("links Replay of to the source build once it loads", async () => {
    vi.mocked(getRun).mockResolvedValue({ ...baseRun, runId: "orig", buildNumber: 7 });
    renderMeta({ ...baseRun, replayOf: "orig" });
    const link = await screen.findByRole("link", { name: "Replay of #7" });
    expect(link).toHaveAttribute("href", "/pipelines/team-a/svc%2Fmain/orig");
  });

  it("names an earlier build when the source read fails", async () => {
    vi.mocked(getRun).mockRejectedValue(new Error("not found"));
    renderMeta({ ...baseRun, replayOf: "gone" });
    expect(await screen.findByText("Replay of an earlier build")).toBeInTheDocument();
  });

  it("renders no replay row without replayOf", () => {
    renderMeta(baseRun);
    expect(screen.queryByText(/Replay of/)).toBeNull();
    expect(getRun).not.toHaveBeenCalled();
  });
});
