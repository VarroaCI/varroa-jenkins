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
        <RunMetadata run={run} tenant="team-a" job="svc/main" />
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

  it("links the run revision on a Gitea host that the run names", () => {
    renderMeta({ ...baseRun, scmUrl: "https://gitea.example.com/acme/app", scmForge: "gitea" });
    expect(screen.getByRole("link", { name: "abcdef12" })).toHaveAttribute(
      "href",
      "https://gitea.example.com/acme/app/commit/abcdef1234567890",
    );
  });

  it("names the trigger source in the actor tooltip", () => {
    renderMeta({ ...baseRun, actor: "alice", triggerSource: "cron" });
    expect(screen.getByText("alice")).toHaveAttribute("title", "Triggered by a schedule");
  });

  it("shows no actor for the unknown placeholder, and reads timer as schedule", () => {
    const { unmount } = renderMeta({ ...baseRun, actor: "unknown" });
    expect(screen.queryByText("unknown")).not.toBeInTheDocument();
    unmount();
    renderMeta({ ...baseRun, actor: "timer" });
    expect(screen.getByText("schedule")).toBeInTheDocument();
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

  it("links Restarted from to the source build and names the stage", async () => {
    vi.mocked(getRun).mockResolvedValue({ ...baseRun, runId: "orig", buildNumber: 7 });
    renderMeta({ ...baseRun, restartOf: "orig", restartStage: "deploy" });
    const link = await screen.findByRole("link", { name: "Restarted from #7 at stage deploy" });
    expect(link).toHaveAttribute("href", "/pipelines/team-a/svc%2Fmain/orig");
  });

  it("renders no replay row without replayOf", () => {
    renderMeta(baseRun);
    expect(screen.queryByText(/Replay of/)).toBeNull();
    expect(getRun).not.toHaveBeenCalled();
  });
});

describe("RunMetadata — libraries", () => {
  beforeEach(() => {
    vi.mocked(getRun).mockReset();
  });

  function withLibraries(libraries: NonNullable<RunDetail["source"]>["libraries"]): RunDetail {
    return { ...baseRun, source: { libraries } } as RunDetail;
  }

  it("links a library at its revision when the forge is known", () => {
    renderMeta(
      withLibraries([
        {
          name: "shared-lib",
          version: "1.2.0",
          revision: "abc1234567890",
          repositoryUrl: "https://github.com/example/shared-lib.git",
          repositoryForge: "github",
        },
      ]),
    );
    expect(screen.getByRole("link", { name: /shared-lib@1\.2\.0 abc12345/ })).toHaveAttribute(
      "href",
      "https://github.com/example/shared-lib/commit/abc1234567890",
    );
  });

  it("links a library at its version when no revision is known", () => {
    renderMeta(
      withLibraries([
        {
          name: "shared-lib",
          version: "release/2",
          repositoryUrl: "https://gitlab.com/example/shared-lib",
          repositoryForge: "gitlab",
        },
      ]),
    );
    expect(screen.getByRole("link", { name: "shared-lib@release/2" })).toHaveAttribute(
      "href",
      "https://gitlab.com/example/shared-lib/-/tree/release/2",
    );
  });

  it("shows text, not a link, for a library with no known forge", () => {
    renderMeta(
      withLibraries([
        {
          name: "shared-lib",
          version: "1.2.0",
          repositoryUrl: "https://git.example.org/team/shared-lib",
        },
      ]),
    );
    expect(screen.getByText("shared-lib@1.2.0")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "shared-lib@1.2.0" })).not.toBeInTheDocument();
  });
});

describe("RunMetadata — pull request", () => {
  it("names the change request and links it when the URL is a web link", () => {
    renderMeta({
      ...baseRun,
      changeRequest: {
        id: "12",
        title: "Fix the cache",
        sourceBranch: "fix/cache",
        targetBranch: "main",
        url: "https://forge.example/pr/12",
      },
    });
    const link = screen.getByRole("link", { name: "#12 Fix the cache (fix/cache → main)" });
    expect(link).toHaveAttribute("href", "https://forge.example/pr/12");
    expect(screen.getByText("Pull request")).toBeInTheDocument();
  });

  it("shows the change request without a link when the URL is not a web link", () => {
    renderMeta({ ...baseRun, changeRequest: { id: "7", url: "javascript:alert(1)" } });
    expect(screen.getByText("#7")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "#7" })).not.toBeInTheDocument();
  });

  it("hides the row for a branch run", () => {
    renderMeta(baseRun);
    expect(screen.queryByText("Pull request")).not.toBeInTheDocument();
  });
});
