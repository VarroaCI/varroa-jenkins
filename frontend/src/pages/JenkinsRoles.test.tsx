import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import JenkinsRoles from "./JenkinsRoles";
import { renderWithProviders } from "../test/render-utils";
import { createJenkinsRole } from "../test/factories";
import type { JenkinsRole } from "../types";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListJenkinsRoles = vi.fn();
const mockDeleteJenkinsRole = vi.fn();
const CORE_CLUSTER = { name: "core", core: true, healthy: true, state: "active" };
const MEMBER_CLUSTER = { name: "member-b", core: false, healthy: true, state: "active" };
const mockClusterList = { items: [CORE_CLUSTER] };

vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve(mockClusterList.items),
  listJenkinsRoles: (...args: unknown[]) => mockListJenkinsRoles(...args),
  deleteJenkinsRole: (...args: unknown[]) => mockDeleteJenkinsRole(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: () => true,
}));

const globalRole = createJenkinsRole({
  metadata: { name: "jenkins-global-reader" },
  spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Read"], description: "Can read everything" },
});

const itemRole = createJenkinsRole({
  metadata: { name: "jenkins-item-builder" },
  spec: { roleType: "Item", permissions: ["hudson.model.Item.Build"], description: "" },
});

describe("JenkinsRoles", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListJenkinsRoles.mockResolvedValue({ items: [globalRole, itemRole] });
  });

  it("shows loading state", () => {
    mockListJenkinsRoles.mockReturnValue(new Promise(() => {}));
    renderWithProviders(<JenkinsRoles />);
    expect(screen.getByText("Loading Jenkins roles...")).toBeInTheDocument();
  });

  it("shows error state", async () => {
    mockListJenkinsRoles.mockRejectedValue(new Error("API unavailable"));
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load/)).toBeInTheDocument();
    });
    expect(screen.getByText(/API unavailable/)).toBeInTheDocument();
  });

  it("shows empty state", async () => {
    mockListJenkinsRoles.mockResolvedValue({ items: [] });
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("No Jenkins roles found.")).toBeInTheDocument();
    });
  });

  it("renders page heading and description", async () => {
    renderWithProviders(<JenkinsRoles />);
    expect(screen.getByText("Jenkins Roles")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/JenkinsRole/)).toBeInTheDocument();
    });
  });

  it("renders a semantic table and places permissions under their role type", async () => {
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
      expect(screen.getByText("jenkins-item-builder")).toBeInTheDocument();
    });
    // Role types — also appear in filter chips, use getAllByText
    expect(screen.getAllByText("Global").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Item").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByRole("table")).toBeInTheDocument();
    const globalRow = screen.getByText("jenkins-global-reader").closest("tr")!;
    expect(within(globalRow).getByText("hudson.model.Hudson.Read")).toHaveAttribute("data-label", "Global");
    const itemRow = screen.getByText("jenkins-item-builder").closest("tr")!;
    expect(within(itemRow).getByText("hudson.model.Item.Build")).toHaveAttribute("data-label", "Item");
    expect(within(itemRow).getByText("core")).toHaveAttribute("data-label", "Cluster");
  });

  it("navigates to create page on New Jenkins Role click", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });
    await user.click(screen.getByText("＋ New Jenkins Role"));
    expect(mockNavigate).toHaveBeenCalledWith("/access/jenkins-roles/create?cluster=core");
  });

  it("navigates to edit page per role", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });
    await user.click(screen.getAllByText("Edit")[0]);
    expect(mockNavigate).toHaveBeenCalledWith(expect.stringContaining("/edit"));
  });

  it("shows View and a Federated badge for a federated copy", async () => {
    const user = userEvent.setup();
    const copyRole = createJenkinsRole({
      metadata: {
        name: "varroa-fed-alpha-0123456789ab",
        labels: { "varroa.dev/federated-from": "alpha" },
        annotations: { "varroa.dev/federated-roles": "alpha,zeta" },
      },
      spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Read"], description: "" },
    });
    mockListJenkinsRoles.mockResolvedValue({ items: [globalRole, copyRole] });
    renderWithProviders(<JenkinsRoles />);
    await screen.findByText("varroa-fed-alpha-0123456789ab");

    const copyName = screen.getByText("varroa-fed-alpha-0123456789ab");
    expect(copyName.closest("td")).toHaveAttribute("data-label", "Name");
    const copyRow = copyName.closest("tr")!;
    expect(within(copyRow).getByText("Federated")).toHaveAttribute(
      "title",
      "Federated copy of VarroaRole alpha, zeta. Change the source on the core cluster.",
    );
    expect(within(copyRow).queryByRole("button", { name: "Edit" })).toBeNull();
    expect(within(copyRow).getByRole("button", { name: "Delete" })).toBeInTheDocument();

    const plainRow = screen.getByText("jenkins-global-reader").closest("tr")!;
    expect(within(plainRow).getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(within(plainRow).queryByText("Federated")).toBeNull();

    await user.click(within(copyRow).getByRole("button", { name: "View" }));
    expect(mockNavigate).toHaveBeenCalledWith(
      "/access/jenkins-roles/varroa-fed-alpha-0123456789ab/edit?cluster=core",
    );
  });

  it("shows delete confirmation and executes deletion", async () => {
    mockDeleteJenkinsRole.mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });

    await user.click(screen.getAllByText("Delete")[0]);
    expect(screen.getByText("Delete Jenkins Role")).toBeInTheDocument();

    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    const confirmBtn = within(cancelBtn.parentElement!).getByText("Delete");
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(mockDeleteJenkinsRole).toHaveBeenCalledWith("core", "jenkins-global-reader");
    });
  });

  it("filters roles by type (Global / Item / Agent)", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
      expect(screen.getByText("jenkins-item-builder")).toBeInTheDocument();
    });

    // Click Item filter chip (button text is "Item N" with count, use regex)
    await user.click(screen.getByRole("button", { name: /^Item/ }));
    expect(screen.queryByText("jenkins-global-reader")).not.toBeInTheDocument();
    expect(screen.getByText("jenkins-item-builder")).toBeInTheDocument();
  });

  it("shows empty filter result message when filter matches nothing", async () => {
    const user = userEvent.setup();
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.getByText("jenkins-global-reader")).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: /^Agent/ }));
    expect(screen.getByText(/No Jenkins roles match the current filters/)).toBeInTheDocument();
  });
});

describe("JenkinsRoles cluster switch", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockClusterList.items = [CORE_CLUSTER, MEMBER_CLUSTER];
  });

  afterEach(() => {
    mockClusterList.items = [CORE_CLUSTER];
  });

  it("keeps the roles of the selected cluster when the read of the previous cluster ends later", async () => {
    let resolveCore!: (value: { items: JenkinsRole[] }) => void;
    const memberRole = createJenkinsRole({
      metadata: { name: "member-b-reader" },
      spec: { roleType: "Global", permissions: ["hudson.model.Hudson.Read"] },
    });
    mockListJenkinsRoles.mockImplementation((cluster: string) =>
      cluster === "core"
        ? new Promise<{ items: JenkinsRole[] }>((resolve) => {
            resolveCore = resolve;
          })
        : Promise.resolve({ items: [memberRole] }),
    );
    renderWithProviders(<JenkinsRoles />);

    await userEvent.selectOptions(await screen.findByRole("combobox"), "member-b");
    await screen.findByText("member-b-reader");

    await act(async () => {
      resolveCore({ items: [globalRole] });
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByText("member-b-reader")).toBeInTheDocument();
    expect(screen.queryByText("jenkins-global-reader")).not.toBeInTheDocument();
  });
});

describe("JenkinsRoles no accessible cluster", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListJenkinsRoles.mockResolvedValue({ items: [globalRole, itemRole] });
    mockClusterList.items = [];
  });

  afterEach(() => {
    mockClusterList.items = [CORE_CLUSTER];
  });

  it("ends the loading state and shows no rows when no cluster is accessible", async () => {
    renderWithProviders(<JenkinsRoles />);
    await waitFor(() => {
      expect(screen.queryByText("Loading Jenkins roles...")).not.toBeInTheDocument();
    });
    expect(screen.queryByText("jenkins-global-reader")).not.toBeInTheDocument();
  });

  it("clears existing roles when no cluster remains", async () => {
    mockClusterList.items = [CORE_CLUSTER];
    const { queryClient } = renderWithProviders(<JenkinsRoles />);
    await screen.findByText("jenkins-global-reader");

    mockClusterList.items = [];
    void queryClient.invalidateQueries({ queryKey: ["clusters"] });

    await waitFor(() => expect(screen.queryByText("jenkins-global-reader")).not.toBeInTheDocument());
  });
});
