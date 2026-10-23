import { useState } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import { createPipelineSummary } from "../test/factories";
import type { TenantScope } from "../types/auth";
import TenantScopeEditor from "./TenantScopeEditor";

const mockListTenants = vi.fn();
const mockListPipelines = vi.fn();
vi.mock("../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/client")>()),
  listTenants: (...args: unknown[]) => mockListTenants(...args),
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
}));

/** Harness holds the editor's value, the way the binding form does. */
function Harness({
  initial,
  onValue,
}: {
  initial?: TenantScope[];
  onValue?: (next: TenantScope[] | undefined) => void;
}) {
  const [value, setValue] = useState<TenantScope[] | undefined>(initial);
  return (
    <TenantScopeEditor
      value={value}
      onChange={(next) => {
        setValue(next);
        onValue?.(next);
      }}
    />
  );
}

describe("TenantScopeEditor", () => {
  beforeEach(() => {
    mockListTenants.mockReset();
    mockListPipelines.mockReset();
    mockListTenants.mockResolvedValue([{ name: "payments", namespaces: ["team-a"] }]);
    mockListPipelines.mockResolvedValue({
      items: [],
      runningNow: [],
      tenantFolder: "payments",
      truncated: false,
    });
  });

  it("authors a tenant scope with a tenant name and a job pattern", async () => {
    const user = userEvent.setup();
    const onValue = vi.fn();
    renderWithProviders(<Harness onValue={onValue} />);

    await user.click(screen.getByRole("button", { name: "+ Add tenant" }));
    await user.type(screen.getByLabelText("Tenant"), "payments");
    await user.click(screen.getByRole("button", { name: "+ Narrow to job patterns" }));
    await user.click(screen.getByRole("button", { name: "+ Add pattern" }));
    await user.type(screen.getByLabelText("Job pattern 1 for entry 1"), "release/*");

    await waitFor(() => {
      expect(onValue).toHaveBeenLastCalledWith([{ name: "payments", jobs: ["release/*"] }]);
    });
  });

  it("accepts a tenant name the list does not hold", async () => {
    const user = userEvent.setup();
    const onValue = vi.fn();
    renderWithProviders(<Harness onValue={onValue} />);

    await user.click(screen.getByRole("button", { name: "+ Add tenant" }));
    await user.type(screen.getByLabelText("Tenant"), "not-yet-created");

    await waitFor(() => {
      expect(onValue).toHaveBeenLastCalledWith([{ name: "not-yet-created" }]);
    });
    expect(screen.queryByText(/must name a tenant/)).not.toBeInTheDocument();
  });

  it("refuses an absolute pattern and names the folder-relative rule", async () => {
    renderWithProviders(<Harness initial={[{ name: "payments", jobs: ["/release"] }]} />);
    await waitFor(() => {
      expect(screen.getByText(/must not start with "\/"/)).toBeInTheDocument();
    });
    expect(screen.getAllByText(/folder-relative/).length).toBeGreaterThan(0);
  });

  it("refuses a jobs list that is present and empty", async () => {
    renderWithProviders(<Harness initial={[{ name: "payments", jobs: [] }]} />);
    await waitFor(() => {
      expect(screen.getByText(/An omitted jobs list covers every job/)).toBeInTheDocument();
    });
  });

  it("counts the matching jobs the caller can see", async () => {
    mockListPipelines.mockResolvedValue({
      items: [
        createPipelineSummary({ jobPath: "payments/release/eu" }),
        createPipelineSummary({ jobPath: "payments/release/us" }),
        createPipelineSummary({ jobPath: "payments/release/apac" }),
        ...Array.from({ length: 7 }, (_, i) =>
          createPipelineSummary({ jobPath: `payments/build/job-${i}` }),
        ),
      ],
      runningNow: [],
      tenantFolder: "payments",
      truncated: false,
    });
    renderWithProviders(<Harness initial={[{ name: "payments", jobs: ["release/*"] }]} />);
    await waitFor(() => {
      expect(screen.getByText("matches 3 of 10 jobs you can see")).toBeInTheDocument();
    });
  });

  it("states that the job list is not available, and still accepts the pattern", async () => {
    for (const status of [404, 409, 422, 503]) {
      mockListPipelines.mockReset();
      mockListPipelines.mockRejectedValue(Object.assign(new Error("no"), { status }));
      const view = renderWithProviders(<Harness initial={[{ name: "payments", jobs: ["release/*"] }]} />);
      await waitFor(() => {
        expect(screen.getByText("The job list is not available.")).toBeInTheDocument();
      });
      expect(screen.getByDisplayValue("release/*")).toBeInTheDocument();
      expect(screen.queryByText(/must not/)).not.toBeInTheDocument();
      view.unmount();
    }
  });

  it("describes a job pattern as a Varroa rule, and claims no Jenkins effect", async () => {
    renderWithProviders(<Harness initial={[{ name: "payments", jobs: ["release/*"] }]} />);
    const help = await screen.findByText(/A job pattern is a Varroa rule/);
    expect(help.textContent).toContain("It changes nothing inside Jenkins.");
    expect(help.textContent).not.toMatch(/Jenkins (role|permission)/);
  });
});
