import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Routes, Route, useLocation } from "react-router-dom";
import { renderWithProviders } from "../../test/render-utils";
import type { Permissions } from "../../types/auth";
import { RunPipelineDialog } from "./RunPipelineDialog";

const mockListPipelines = vi.fn();
vi.mock("../../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/client")>()),
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
}));

/** item builds one job row of the tenant list. */
function item(jobPath: string, kind: "pipeline" | "multibranch" = "pipeline") {
  return {
    jobPath,
    kind,
    pending: false,
    health: { bucket: "sunny", window: 0, counted: 0, passed: 0, score: 0 },
  };
}

function listing(items: ReturnType<typeof item>[]) {
  return { items, runningNow: [], tenantFolder: "", truncated: false, retention: {} };
}

/** Where shows the path the router holds after Continue. */
function Where() {
  const location = useLocation();
  return <div data-testid="where">{location.pathname + location.search}</div>;
}

/** WRITER grants write on payments and member-b, with no job pattern. */
const WRITER = {
  global: {},
  scopes: [],
  tenants: [
    { tenant: "payments", jobs: [], capabilities: { pipelines: { write: true } } },
    { tenant: "member-b", jobs: [], capabilities: { pipelines: { write: true } } },
  ],
} as unknown as Permissions;

function renderDialog(tenants: string[], onClose = vi.fn(), permissions: Permissions = WRITER) {
  return {
    onClose,
    ...renderWithProviders(
      <Routes>
        <Route
          path="/"
          element={<RunPipelineDialog tenants={tenants} permissions={permissions} onClose={onClose} />}
        />
        <Route path="*" element={<Where />} />
      </Routes>,
      { route: "/" },
    ),
  };
}

describe("RunPipelineDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListPipelines.mockResolvedValue(listing([item("checkout"), item("reports"), item("pr-scan", "multibranch")]));
  });

  it("lists only tenants the caller may write", () => {
    renderDialog(["payments", "member-b"]);
    const select = screen.getByRole("combobox", { name: "Tenant" });
    const names = within(select)
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(names).toEqual(["Choose a tenant", "payments", "member-b"]);
  });

  it("preselects the one writable tenant and reads its jobs", async () => {
    renderDialog(["payments"]);
    expect(screen.getByRole("combobox", { name: "Tenant" })).toHaveValue("payments");
    await waitFor(() => expect(mockListPipelines).toHaveBeenCalledWith("payments"));
  });

  it("lists only pipeline jobs of the chosen tenant", async () => {
    renderDialog(["payments", "member-b"]);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Tenant" }), "payments");
    const jobs = await screen.findByRole("option", { name: "checkout" });
    expect(jobs).toBeInTheDocument();
    const options = within(screen.getByRole("combobox", { name: "Job" }))
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(options).toEqual(["Choose a job", "checkout", "reports"]);
  });

  it("hides a job that a job-pattern grant excludes", async () => {
    const pattern = {
      global: {},
      scopes: [],
      tenants: [{ tenant: "payments", jobs: ["checkout"], capabilities: { pipelines: { write: true } } }],
    } as unknown as Permissions;
    renderDialog(["payments"], vi.fn(), pattern);
    await screen.findByRole("option", { name: "checkout" });
    const options = within(screen.getByRole("combobox", { name: "Job" }))
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(options).toEqual(["Choose a job", "checkout"]);
  });

  it("keeps Continue disabled until a job is chosen", async () => {
    renderDialog(["payments"]);
    const cont = screen.getByRole("button", { name: "Continue" });
    expect(cont).toBeDisabled();
    await screen.findByRole("option", { name: "checkout" });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Job" }), "reports");
    expect(cont).toBeEnabled();
  });

  it("Continue opens the job page with run=1", async () => {
    const { onClose } = renderDialog(["payments"]);
    await screen.findByRole("option", { name: "checkout" });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Job" }), "checkout");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(screen.getByTestId("where").textContent).toBe("/pipelines/payments/checkout?run=1");
    expect(onClose).toHaveBeenCalled();
  });

  it("says a tenant with no pipeline job has none", async () => {
    mockListPipelines.mockResolvedValue(listing([item("pr-scan", "multibranch")]));
    renderDialog(["payments"]);
    expect(await screen.findByText("This tenant has no pipeline job.")).toBeInTheDocument();
  });

  it("says the jobs did not load and Retry reads again", async () => {
    mockListPipelines.mockRejectedValueOnce(new Error("boom")).mockResolvedValue(listing([item("checkout")]));
    renderDialog(["payments"]);
    expect(await screen.findByText("Varroa could not load the jobs.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("option", { name: "checkout" })).toBeInTheDocument();
  });

  it("Escape closes the dialog", () => {
    const { onClose } = renderDialog(["payments"]);
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });
});
