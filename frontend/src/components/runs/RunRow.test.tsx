import fs from "fs";
import path from "path";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { FleetRunRow } from "../../types";
import { RunRow } from "./RunRow";
import { DUPLICATE_RUN_NOTE } from "./runGlyph";
import styles from "./RunRow.module.css";

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
    changeSummary: "Fix the parser",
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
    renderRow(row({ changeSummary: undefined }));
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

  it("marks a duplicate run Duplicate, with its note, and offers Re-run but no Cancel", async () => {
    const user = userEvent.setup();
    renderRow(row({ status: "DUPLICATE", result: undefined, buildNumber: undefined, durationMs: undefined }));
    const mark = screen.getByRole("img", { name: "Duplicate" });
    expect(mark).toHaveAttribute("title", DUPLICATE_RUN_NOTE);
    expect(screen.queryByText(/queued|running/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Actions for/ }));
    expect(screen.getByRole("menuitem", { name: "Re-run" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Cancel" })).not.toBeInTheDocument();
  });

  it("keeps the Duplicate mark for a duplicate run that stores a result", () => {
    renderRow(row({ status: "DUPLICATE", result: "ABORTED" }));
    expect(screen.getByRole("img", { name: "Duplicate" })).toBeInTheDocument();
    expect(screen.queryByRole("img", { name: "Aborted" })).not.toBeInTheDocument();
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

describe("RunRow phone layout", () => {
  const css = fs.readFileSync(path.join(__dirname, "RunRow.module.css"), "utf-8");
  const phone = css.slice(css.indexOf("@media (max-width: 639.98px)"));

  it("resets each stacked cell with a selector that outranks the shared cell rule", () => {
    expect(phone).toMatch(/tr\.row > td \{[^}]*padding: 0;[^}]*border: 0;/);
    expect(phone).not.toMatch(/\n\s*\.row td \{/);
  });

  it("hides an empty stacked cell", () => {
    expect(phone).toMatch(/tr\.row > td:empty \{[^}]*display: none;/);
  });

  it("renders the actor and branch cells empty for a machine run with no branch", () => {
    const { container } = renderRow(row({ actor: "timer", branch: undefined }));
    expect(container.querySelector(`.${styles.actorCell}`)).toBeEmptyDOMElement();
    expect(container.querySelector(`.${styles.branchCell}`)).toBeEmptyDOMElement();
  });

  it("shows the start age once for a completed run", () => {
    const { container } = renderRow(
      row({ startedAt: new Date(NOW - 3_600_000).toISOString(), durationMs: 75_000 }),
    );
    expect(container.textContent?.match(/1h ago/g)).toHaveLength(1);
    expect(screen.getByText("1m 15s")).toBeInTheDocument();
  });
});
