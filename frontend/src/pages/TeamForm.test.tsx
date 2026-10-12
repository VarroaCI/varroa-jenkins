import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import TeamForm from "./TeamForm";

const mockNavigate = vi.fn();
let mockParams: { name?: string } = {};
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useNavigate: () => mockNavigate,
    useParams: () => mockParams,
  };
});

const mockGetTeam = vi.fn();
const mockCreateTeam = vi.fn();
const mockUpdateTeam = vi.fn();
const mockListRoles = vi.fn();
const mockListComposedBundles = vi.fn();
vi.mock("../api/client", () => ({
  getTeam: (...args: unknown[]) => mockGetTeam(...args),
  createTeam: (...args: unknown[]) => mockCreateTeam(...args),
  updateTeam: (...args: unknown[]) => mockUpdateTeam(...args),
  listRoles: (...args: unknown[]) => mockListRoles(...args),
  listComposedBundles: (...args: unknown[]) => mockListComposedBundles(...args),
}));

const mockUseClustersData = {
  data: [{ name: "core", core: true }],
};
vi.mock("../hooks/useClusters", () => ({
  useClusters: () => mockUseClustersData,
}));

const bundleList = {
  items: [
    {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "ComposedBundle",
      metadata: { name: "starter", namespace: "team-alpha" },
      spec: { inputs: [] },
    },
  ],
};

describe("TeamForm", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockParams = {};
    mockListRoles.mockResolvedValue({ items: [{ metadata: { name: "developer" } }] });
    mockListComposedBundles.mockResolvedValue(bundleList);
    mockCreateTeam.mockResolvedValue({ name: "alpha-team" });
    mockUpdateTeam.mockResolvedValue({ name: "alpha-team" });
  });

  it("emits pipelines with the selected bundle's namespace explicitly", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "alpha-team");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "alice");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "team-alpha");

    await user.click(screen.getByLabelText(/Pipelines tenant/));
    await user.type(screen.getByPlaceholderText("JenkinsVersionProfile name"), "lts-2.479");
    await user.type(screen.getByPlaceholderText("CellPool name (optional)"), "solo");

    await user.selectOptions(screen.getByText("Select a namespace").closest("select")!, "team-alpha");
    await user.selectOptions(screen.getByText("Select a cluster").closest("select")!, "core");

    await waitFor(() => {
      expect(mockListComposedBundles).toHaveBeenCalledWith("core");
    });

    const bundleSelect = screen.getByText("No bundle").closest("select")!;
    await waitFor(() => {
      expect(screen.getByText("team-alpha/starter")).toBeInTheDocument();
    });
    await user.selectOptions(bundleSelect, "team-alpha::starter");

    await user.click(screen.getByText("Create"));

    await waitFor(() => {
      expect(mockCreateTeam).toHaveBeenCalled();
    });
    const body = mockCreateTeam.mock.calls[0][0];
    expect(body.pipelines).toEqual({
      profile: "lts-2.479",
      cellPool: "solo",
      agentNamespace: "team-alpha",
      composedBundleRef: { name: "starter", namespace: "team-alpha" },
      interruptedRetry: "manual",
      concurrency: "allow",
      cronBackstop: true,
      cronCatchUp: "one",
    });
  });

  it("emits the seven pipelines ledger and rehydration settings when changed", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "gamma-team");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "carol");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "team-gamma");

    await user.click(screen.getByLabelText(/Pipelines tenant/));
    await user.type(screen.getByPlaceholderText("JenkinsVersionProfile name"), "lts");
    await user.selectOptions(screen.getByText("Select a namespace").closest("select")!, "team-gamma");

    await user.type(screen.getByPlaceholderText("10"), "5");
    await user.type(screen.getByPlaceholderText("30m"), "45m");
    await user.selectOptions(screen.getByText("Manual").closest("select")!, "automatic");
    await user.selectOptions(screen.getByText("Allow (run side by side)").closest("select")!, "cancelInProgress");
    await user.click(screen.getByLabelText(/Backstop cron scheduler/));
    await user.selectOptions(screen.getByText("Fire newest missed occurrence").closest("select")!, "none");
    await user.type(screen.getByPlaceholderText("5m"), "10m");

    await user.click(screen.getByText("Create"));

    await waitFor(() => {
      expect(mockCreateTeam).toHaveBeenCalled();
    });
    const body = mockCreateTeam.mock.calls[0][0];
    expect(body.pipelines).toMatchObject({
      rehydrateBuilds: 5,
      rehydrateDeadline: "45m",
      interruptedRetry: "automatic",
      concurrency: "cancelInProgress",
      cronBackstop: false,
      cronCatchUp: "none",
      cronGrace: "10m",
    });
  });

  it("shows the placement on the edit view", async () => {
    mockParams = { name: "alpha-team" };
    mockGetTeam.mockResolvedValue({
      name: "alpha-team",
      members: ["alice"],
      namespaces: ["team-alpha"],
      roleRef: "developer",
      pipelines: { profile: "lts-2.479", agentNamespace: "team-alpha" },
      placement: { cluster: "core", cell: "cell-lts-0", cellNamespace: "varroa-cells", generation: 2 },
      conditions: [{ type: "Placed", status: "True", reason: "TenantPlaced" }],
    });

    render(<TeamForm />);

    await waitFor(() => {
      expect(screen.getByText(/core\/varroa-cells\/cell-lts-0/)).toBeInTheDocument();
    });
    expect(screen.getByText("Placed")).toBeInTheDocument();
  });

  it("shows the rehydration status on the edit view", async () => {
    mockParams = { name: "alpha-team" };
    mockGetTeam.mockResolvedValue({
      name: "alpha-team",
      members: ["alice"],
      namespaces: ["team-alpha"],
      roleRef: "developer",
      pipelines: { profile: "lts-2.479", agentNamespace: "team-alpha" },
      placement: { cluster: "core", cell: "cell-lts-0", cellNamespace: "varroa-cells", generation: 2 },
      conditions: [
        { type: "Placed", status: "True", reason: "TenantPlaced" },
        { type: "Rehydrated", status: "False", reason: "RehydrationPending" },
      ],
      rehydration: { records: 8, truncated: false, unseededJobs: 1 },
    });

    render(<TeamForm />);

    await waitFor(() => {
      expect(screen.getByText("RehydrationPending")).toBeInTheDocument();
    });
    expect(screen.getByText(/8 record\(s\).*1 job\(s\) unseeded/)).toBeInTheDocument();
  });

  it("lists tenant warnings on the edit view", async () => {
    mockParams = { name: "alpha-team" };
    mockGetTeam.mockResolvedValue({
      name: "alpha-team",
      members: ["alice"],
      namespaces: ["team-alpha"],
      roleRef: "developer",
      pipelines: { profile: "lts-2.479", agentNamespace: "team-alpha" },
      placement: { cluster: "core", cell: "cell-lts-0", cellNamespace: "varroa-cells", generation: 1 },
      conditions: [
        { type: "Placed", status: "True", reason: "TenantPlaced" },
        {
          type: "TenantWarnings",
          status: "True",
          reason: "BundleKeysIgnored",
          message: "alpha-team: BundleKeysIgnored: jenkins.yaml keys were dropped",
        },
      ],
    });

    render(<TeamForm />);

    await waitFor(() => {
      expect(screen.getByText("BundleKeysIgnored")).toBeInTheDocument();
    });
    expect(
      screen.getByText("alpha-team: BundleKeysIgnored: jenkins.yaml keys were dropped"),
    ).toBeInTheDocument();
  });

  it("emits a job export with its callers", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "payments");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "alice");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "ns1");

    await user.click(screen.getByLabelText(/Pipelines tenant/));
    await user.type(screen.getByPlaceholderText("JenkinsVersionProfile name"), "lts");
    await user.selectOptions(screen.getByText("Select a namespace").closest("select")!, "ns1");
    await user.click(screen.getByText("+ Add export"));
    await user.type(screen.getByPlaceholderText("release/*"), "release/*");
    await user.type(screen.getByPlaceholderText("Team name"), "search");

    await user.click(screen.getByText("Create"));

    await waitFor(() => {
      expect(mockCreateTeam).toHaveBeenCalled();
    });
    const body = mockCreateTeam.mock.calls[0][0];
    expect(body.pipelines.exports).toEqual([{ job: "release/*", callers: ["search"] }]);
  });

  it("emits a locks export with its callers", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "payments");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "alice");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "ns1");

    await user.click(screen.getByLabelText(/Pipelines tenant/));
    await user.type(screen.getByPlaceholderText("JenkinsVersionProfile name"), "lts");
    await user.selectOptions(screen.getByText("Select a namespace").closest("select")!, "ns1");
    await user.click(screen.getByText("+ Add export"));
    await user.selectOptions(screen.getByText("Job pattern").closest("select")!, "locks");
    await user.type(screen.getByPlaceholderText("deploy-prod"), "deploy-prod");
    await user.type(screen.getByPlaceholderText("Team name"), "alpha");

    await user.click(screen.getByText("Create"));

    await waitFor(() => {
      expect(mockCreateTeam).toHaveBeenCalled();
    });
    const body = mockCreateTeam.mock.calls[0][0];
    expect(body.pipelines.exports).toEqual([{ locks: ["deploy-prod"], callers: ["alpha"] }]);
  });

  it("refuses a pipelines tenant with no version profile or agent namespace", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "payments");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "alice");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "ns1");

    await user.click(screen.getByLabelText(/Pipelines tenant/));
    await user.click(screen.getByText("Create"));

    expect(await screen.findByText("Version profile is required")).toBeInTheDocument();
    expect(screen.getByText("Agent namespace is required")).toBeInTheDocument();
    expect(mockCreateTeam).not.toHaveBeenCalled();
  });

  it("marks the version profile and agent namespace as required", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    await user.click(screen.getByLabelText(/Pipelines tenant/));

    expect(screen.getByPlaceholderText("JenkinsVersionProfile name")).toHaveAttribute("aria-required", "true");
    expect(screen.getByText("Select a namespace").closest("select")).toHaveAttribute("aria-required", "true");
  });

  it("rejects a submitted export that names neither a job pattern nor a caller", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "payments");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "alice");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "ns1");

    await user.click(screen.getByLabelText(/Pipelines tenant/));
    await user.click(screen.getByText("+ Add export"));

    await user.click(screen.getByText("Create"));

    expect(await screen.findByText("Export 1: a job pattern is required")).toBeInTheDocument();
    expect(screen.getByText("Export 1: at least one caller is required")).toBeInTheDocument();
    expect(mockCreateTeam).not.toHaveBeenCalled();
  });

  it("loads existing exports of both kinds on the edit view", async () => {
    mockParams = { name: "payments" };
    mockGetTeam.mockResolvedValue({
      name: "payments",
      members: ["alice"],
      namespaces: ["ns1"],
      roleRef: "developer",
      pipelines: {
        profile: "lts",
        agentNamespace: "ns1",
        exports: [
          { job: "release/*", callers: ["payments", "search"] },
          { locks: ["deploy-prod"], callers: ["alpha"] },
        ],
      },
    });

    render(<TeamForm />);

    await waitFor(() => {
      expect(screen.getByDisplayValue("release/*")).toBeInTheDocument();
    });
    expect(screen.getByDisplayValue("deploy-prod")).toBeInTheDocument();
    expect(screen.getByDisplayValue("payments")).toBeInTheDocument();
    expect(screen.getByDisplayValue("alpha")).toBeInTheDocument();
  });

  it("keeps pipelines.retention on save", async () => {
    const user = userEvent.setup();
    mockParams = { name: "alpha-team" };
    mockGetTeam.mockResolvedValue({
      name: "alpha-team",
      members: ["alice"],
      namespaces: ["team-alpha"],
      roleRef: "developer",
      pipelines: {
        profile: "lts",
        agentNamespace: "team-alpha",
        retention: { runs: "30d", jobs: [{ job: "pr-*", runs: "7d" }] },
      },
    });

    render(<TeamForm />);
    await waitFor(() => {
      expect(screen.getByDisplayValue("alice")).toBeInTheDocument();
    });

    await user.click(screen.getByText("Update"));

    await waitFor(() => {
      expect(mockUpdateTeam).toHaveBeenCalled();
    });
    const body = mockUpdateTeam.mock.calls[0][1];
    expect(body.pipelines.retention).toEqual({ runs: "30d", jobs: [{ job: "pr-*", runs: "7d" }] });
  });

  it("omits pipelines when the tenant toggle stays off", async () => {
    const user = userEvent.setup();
    render(<TeamForm />);

    const nameInput = screen.getByText("Name").nextElementSibling as HTMLInputElement;
    await user.type(nameInput, "beta-team");
    await user.type(screen.getAllByPlaceholderText("Username")[0], "bob");
    await user.type(screen.getAllByPlaceholderText("Namespace")[0], "team-beta");

    await user.click(screen.getByText("Create"));

    await waitFor(() => {
      expect(mockCreateTeam).toHaveBeenCalled();
    });
    const body = mockCreateTeam.mock.calls[0][0];
    expect(body.pipelines).toBeUndefined();
  });

  it("renders the create form inside a card with a description", () => {
    render(<TeamForm />);

    expect(screen.getByRole("heading", { name: "Create Team" })).toBeInTheDocument();
    expect(
      screen.getByText("Define the namespaces, role, and pipelines tenant of a team"),
    ).toBeInTheDocument();
    const nameInput = screen.getByText("Name").nextElementSibling as HTMLElement;
    expect(nameInput.closest('[class*="card"]')).not.toBeNull();
  });

  it("shows the pipelines status card after the form card on the edit view", async () => {
    mockParams = { name: "alpha-team" };
    mockGetTeam.mockResolvedValue({
      name: "alpha-team",
      members: ["alice"],
      namespaces: ["team-alpha"],
      roleRef: "developer",
      pipelines: { profile: "lts-2.479", agentNamespace: "team-alpha" },
      placement: { cluster: "core", cell: "cell-lts-0", cellNamespace: "varroa-cells", generation: 2 },
      conditions: [{ type: "Placed", status: "True", reason: "TenantPlaced" }],
    });

    render(<TeamForm />);

    const status = await screen.findByText("Pipelines status");
    const updateButton = screen.getByRole("button", { name: "Update" });
    const statusCard = status.closest('[class*="card"]');
    expect(statusCard).not.toBeNull();
    expect(statusCard!.contains(updateButton)).toBe(false);
  });
});
