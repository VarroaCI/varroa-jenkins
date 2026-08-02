import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("../api/client", () => ({
  listUsers: vi.fn(),
  createUser: vi.fn(),
  deleteUser: vi.fn(),
  updateUser: vi.fn(),
  adminResetPassword: vi.fn(),
  adminListUserApiKeys: vi.fn(),
  adminRevokeApiKey: vi.fn(),
  listRoleBindings: vi.fn(),
  listJenkinsRoleBindings: vi.fn(),
  listGroups: vi.fn(),
  createGroup: vi.fn(),
}));
vi.mock("../context/AuthContext", () => ({ useAuth: vi.fn() }));

import * as client from "../api/client";
import { useAuth } from "../context/AuthContext";
import SettingsUsersTab from "./SettingsUsersTab";

const m = (fn: unknown) => fn as ReturnType<typeof vi.fn>;
const mockUseAuth = useAuth as ReturnType<typeof vi.fn>;

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SettingsUsersTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  m(client.listGroups).mockResolvedValue([{ name: "devs", displayName: "Developers", members: [] }]);
  m(client.adminListUserApiKeys).mockResolvedValue({ items: [] });
  m(client.createUser).mockResolvedValue({ name: "x" });
  m(client.updateUser).mockResolvedValue({ name: "x" });
  m(client.deleteUser).mockResolvedValue(undefined);
  m(client.adminResetPassword).mockResolvedValue(undefined);
  m(client.createGroup).mockResolvedValue({ name: "devs" });
  m(client.listRoleBindings).mockResolvedValue({ items: [] });
  m(client.listJenkinsRoleBindings).mockResolvedValue({ items: [] });
});

describe("SettingsUsersTab", () => {
  it("local mode: creates a user with an initial group membership", async () => {
    mockUseAuth.mockReturnValue({ authMode: "local" });
    m(client.listUsers).mockResolvedValue([
      { name: "alice", email: "alice@example.com", displayName: "Alice", groups: ["devs"], managedBy: "local" },
    ]);
    renderTab();

    await screen.findByText("alice");
    await userEvent.click(screen.getByText("+ New User"));
    await userEvent.type(screen.getByPlaceholderText("alice"), "carol");
    await userEvent.type(screen.getByPlaceholderText("Min 8 characters"), "password1");
    await userEvent.click(screen.getByLabelText("Developers")); // select group
    await userEvent.click(screen.getByText("Create"));

    await waitFor(() => expect(client.createUser).toHaveBeenCalled());
    expect(m(client.createUser).mock.calls[0][0]).toMatchObject({ username: "carol" });
    // Initial membership applied via create-then-patch.
    await waitFor(() => expect(client.createGroup).toHaveBeenCalled());
  });

  it("local mode: edits a user and resets password from the detail panel", async () => {
    mockUseAuth.mockReturnValue({ authMode: "local" });
    m(client.listUsers).mockResolvedValue([
      { name: "alice", email: "alice@example.com", displayName: "Alice", groups: [], managedBy: "local" },
    ]);
    renderTab();

    await screen.findByText("alice");
    await userEvent.click(screen.getByRole("button", { name: "alice" }));

    // Edit identity + toggle a group membership.
    await screen.findByText("Edit — alice");
    const display = screen.getByDisplayValue("Alice");
    await userEvent.clear(display);
    await userEvent.type(display, "Alice B");
    await userEvent.click(screen.getByLabelText("Developers")); // add to group
    await userEvent.click(screen.getByText("Save changes"));
    await waitFor(() => expect(client.updateUser).toHaveBeenCalledWith("alice", expect.objectContaining({ displayName: "Alice B" })));
    // Membership change is applied via group upsert.
    await waitFor(() => expect(client.createGroup).toHaveBeenCalled());

    // Reset password.
    await userEvent.type(screen.getByPlaceholderText("New password (min 8 chars)"), "newpass12");
    await userEvent.click(screen.getByText("Reset"));
    await waitFor(() => expect(client.adminResetPassword).toHaveBeenCalledWith("alice", "newpass12"));
  });

  it("local mode: delete shows binding preview then deprovisions", async () => {
    mockUseAuth.mockReturnValue({ authMode: "local" });
    m(client.listUsers).mockResolvedValue([
      { name: "alice", email: "alice@example.com", displayName: "Alice", groups: [], managedBy: "local" },
    ]);
    m(client.listRoleBindings).mockResolvedValue({
      items: [{ metadata: { name: "vrb1" }, spec: { subjects: [{ kind: "User", name: "alice" }] } }],
    });
    renderTab();

    await screen.findByText("alice");
    await userEvent.click(screen.getByText("Delete"));
    // Dialog opens; the preview is built from the role-binding lists.
    await screen.findByText("Delete alice?");
    await waitFor(() => expect(client.listRoleBindings).toHaveBeenCalled());
    // The confirm dialog renders before the table, so its Delete button is first.
    const deletes = screen.getAllByRole("button", { name: /Delete/ });
    await userEvent.click(deletes[0]);
    await waitFor(() => expect(client.deleteUser).toHaveBeenCalledWith("alice"));
  });

  it("local mode: lists and revokes a user's API key", async () => {
    mockUseAuth.mockReturnValue({ authMode: "local" });
    m(client.listUsers).mockResolvedValue([
      { name: "alice", email: "a@example.com", displayName: "Alice", groups: [], managedBy: "local" },
    ]);
    m(client.adminListUserApiKeys).mockResolvedValue({ items: [{ prefix: "ak_abc", created: "2026-01-01", expires: "" }] });
    m(client.adminRevokeApiKey).mockResolvedValue(undefined);
    renderTab();

    await screen.findByText("alice");
    await userEvent.click(screen.getByRole("button", { name: "alice" }));
    const revoke = await screen.findByText("Revoke");
    await userEvent.click(revoke);
    await waitFor(() => expect(client.adminRevokeApiKey).toHaveBeenCalledWith("alice", "ak_abc"));
  });

  it("OIDC mode: read-only directory, no create/edit/reset", async () => {
    mockUseAuth.mockReturnValue({ authMode: "oidc" });
    m(client.listUsers).mockResolvedValue([
      { name: "oidc-abc", email: "x@example.com", displayName: "X", groups: ["team-a"], managedBy: "idp" },
    ]);
    renderTab();

    await screen.findByText(/read-only directory \(OIDC mode\)/);
    expect(screen.queryByText("+ New User")).not.toBeInTheDocument();
    expect(client.listGroups).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "oidc-abc" }));
    expect(await screen.findByText("No API keys.")).toBeInTheDocument();
    expect(screen.queryByText("Edit — oidc-abc")).not.toBeInTheDocument();
    expect(screen.queryByText("Reset")).not.toBeInTheDocument();
  });
});
