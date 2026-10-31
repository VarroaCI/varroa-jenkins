import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { FleetRunRow } from "../../types";
import { RunRow } from "./RunRow";

const NOW = Date.parse("2026-10-30T12:00:00Z");

function row(over: Partial<FleetRunRow> = {}): FleetRunRow {
  return {
    runId: "0192f",
    status: "COMPLETED",
    result: "SUCCESS",
    cluster: "core",
    tenant: "pay",
    tenantFolder: "pay",
    jobPath: "pay/api",
    buildNumber: 42,
    actor: "nate",
    branch: "main",
    title: "Fix the parser",
    trigger: { kind: "push", label: "Push" },
    pendingInput: false,
    canWrite: true,
    createdAt: new Date(NOW - 600_000).toISOString(),
    ...over,
  };
}

function renderRow(run: FleetRunRow, handlers = { onRerun: vi.fn(), onCancel: vi.fn() }) {
  return render(
    <MemoryRouter>
      <table>
        <tbody>
          <RunRow run={run} now={NOW} localCluster="core" onRerun={handlers.onRerun} onCancel={handlers.onCancel} />
        </tbody>
      </table>
    </MemoryRouter>,
  );
}

describe("RunRow", () => {
  it("links the title to the build page", () => {
    renderRow(row());
    expect(screen.getByRole("link", { name: "Fix the parser" })).toHaveAttribute(
      "href",
      "/pipelines/pay/pay%2Fapi/0192f",
    );
  });

  it("links a run with no commit subject by its job and build number", () => {
    renderRow(row({ title: "pay/api" }));
    expect(screen.getByRole("link", { name: "api #42" })).toBeInTheDocument();
  });

  it("shows no actor text for an unknown actor", () => {
    renderRow(row({ actor: "unknown" }));
    expect(screen.queryByText("unknown")).not.toBeInTheDocument();
  });

  it("names the state in the status cell", () => {
    renderRow(row());
    expect(screen.getByLabelText("Success")).toBeInTheDocument();
  });

  it("shows the cluster chip only for a run on another cluster", () => {
    renderRow(row({ cluster: "edge" }));
    expect(screen.getByText("edge")).toBeInTheDocument();
  });

  it("shows no cluster chip for a local run", () => {
    renderRow(row({ cluster: "core" }));
    expect(screen.queryByText("core")).not.toBeInTheDocument();
  });

  it("shows Cancel only for a writable running run", async () => {
    const user = userEvent.setup();
    renderRow(row({ status: "RUNNING", result: undefined }));
    await user.click(screen.getByRole("button", { name: /Actions for/ }));
    expect(screen.getByRole("menuitem", { name: "Cancel" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Re-run" })).not.toBeInTheDocument();
  });

  it("shows Re-run only for a writable completed run", async () => {
    const user = userEvent.setup();
    renderRow(row());
    await user.click(screen.getByRole("button", { name: /Actions for/ }));
    expect(screen.getByRole("menuitem", { name: "Re-run" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Cancel" })).not.toBeInTheDocument();
  });

  it("hides every write action when canWrite is false", async () => {
    const user = userEvent.setup();
    renderRow(row({ canWrite: false }));
    await user.click(screen.getByRole("button", { name: /Actions for/ }));
    expect(screen.getByRole("menuitem", { name: "Open" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Re-run" })).not.toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Cancel" })).not.toBeInTheDocument();
  });

  it("calls onRerun with the row", async () => {
    const user = userEvent.setup();
    const handlers = { onRerun: vi.fn(), onCancel: vi.fn() };
    const r = row();
    renderRow(r, handlers);
    await user.click(screen.getByRole("button", { name: /Actions for/ }));
    await user.click(screen.getByRole("menuitem", { name: "Re-run" }));
    expect(handlers.onRerun).toHaveBeenCalledWith(r);
  });
});
