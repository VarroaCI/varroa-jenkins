import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import RoleBindings from "./RoleBindings";
import { renderWithProviders } from "../test/render-utils";
import { createRoleBinding } from "../test/factories";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockListRoleBindings = vi.fn();
const mockDeleteRoleBinding = vi.fn();
vi.mock("../api/client", () => ({
  listRoleBindings: (...args: unknown[]) => mockListRoleBindings(...args),
  deleteRoleBinding: (...args: unknown[]) => mockDeleteRoleBinding(...args),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: () => true,
}));

const agentBinding = createRoleBinding({
  metadata: {
    name: "tenantagent-0eb3bb0eec318ce4",
    labels: {
      "varroa.dev/tenant-agent": "true",
      "varroa.dev/tenant-agent-namespace": "ns-a",
      "varroa.dev/tenant-agent-name": "agent-a",
    },
  },
  spec: {
    subjects: [{ kind: "User", name: "agent:ns-a/tenant-a/agent-a" }],
    roleRef: "developer",
    scope: { tenants: [{ name: "tenant-a" }] },
  },
});

const scheduleBinding = createRoleBinding({
  metadata: {
    name: "broodschedule-de59698b5963b41984ed191b495eeae7fd55f8d8a1eb7876",
    annotations: {
      "varroa.dev/broodschedule-namespace": "ns-a",
      "varroa.dev/broodschedule-name": "nightly",
    },
  },
  spec: {
    subjects: [{ kind: "User", name: "schedule:ns-a/nightly" }],
    roleRef: "operator",
    scope: { namespaces: ["ns-a"] },
  },
});

const renamedCopy = createRoleBinding({
  metadata: {
    name: "copy-of-agent",
    labels: {
      "varroa.dev/tenant-agent": "true",
      "varroa.dev/tenant-agent-namespace": "ns-a",
      "varroa.dev/tenant-agent-name": "agent-a",
    },
  },
  spec: { subjects: [{ kind: "User", name: "bob" }], roleRef: "developer" },
});

describe("RoleBindings — operator-made bindings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListRoleBindings.mockResolvedValue({ items: [agentBinding, scheduleBinding, renamedCopy] });
  });

  it("links the owner badge of each kind to its page", async () => {
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByRole("link", { name: "Tenant agent: ns-a/agent-a" })).toHaveAttribute(
        "href",
        "/pipelines/tenant-a/agent",
      );
    });
    expect(screen.getByRole("link", { name: "Brood schedule: ns-a/nightly" })).toHaveAttribute(
      "href",
      "/brood-schedules/ns-a/nightly",
    );
  });

  it("shows View for an owned binding and keeps Delete", async () => {
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByRole("link", { name: "Brood schedule: ns-a/nightly" })).toBeInTheDocument();
    });
    // The renamed copy has no owner, so its row still reads Edit.
    expect(screen.getAllByText("View")).toHaveLength(2);
    expect(screen.getAllByText("Edit")).toHaveLength(1);
    expect(screen.getAllByText("Delete")).toHaveLength(3);
  });

  it("gives a renamed copy no owner badge", async () => {
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("copy-of-agent")).toBeInTheDocument();
    });
    expect(screen.getAllByRole("link").filter((a) => a.textContent?.startsWith("Tenant agent"))).toHaveLength(1);
  });

  it("filters by owner text", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RoleBindings />);
    await waitFor(() => {
      expect(screen.getByText("copy-of-agent")).toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("Filter by name or owner..."), "nightly");
    await waitFor(() => {
      expect(screen.queryByText("copy-of-agent")).not.toBeInTheDocument();
    });
    expect(screen.getByRole("link", { name: "Brood schedule: ns-a/nightly" })).toBeInTheDocument();
  });
});
