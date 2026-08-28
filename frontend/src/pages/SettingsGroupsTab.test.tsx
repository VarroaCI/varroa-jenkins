import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

vi.mock("../api/client", () => ({
  listGroups: vi.fn(),
  createGroup: vi.fn(),
  deleteGroup: vi.fn(),
  listUsers: vi.fn(),
}));
vi.mock("../context/AuthContext", () => ({ useAuth: vi.fn() }));

import { listGroups, createGroup, deleteGroup, listUsers } from "../api/client";
import { useAuth } from "../context/AuthContext";
import SettingsGroupsTab from "./SettingsGroupsTab";

const mockListGroups = listGroups as ReturnType<typeof vi.fn>;
const mockCreateGroup = createGroup as ReturnType<typeof vi.fn>;
const mockDeleteGroup = deleteGroup as ReturnType<typeof vi.fn>;
const mockListUsers = listUsers as ReturnType<typeof vi.fn>;
const mockUseAuth = useAuth as ReturnType<typeof vi.fn>;

function renderTab(route = "/access/groups") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={qc}>
        <SettingsGroupsTab />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListGroups.mockReset();
  mockCreateGroup.mockReset();
  mockDeleteGroup.mockReset();
  mockListUsers.mockReset();
  mockUseAuth.mockReset();
  mockCreateGroup.mockResolvedValue({ name: "x" });
  mockDeleteGroup.mockResolvedValue(undefined);
});

describe("SettingsGroupsTab", () => {
  it("local mode: lists groups and creates one", async () => {
    mockUseAuth.mockReturnValue({ authMode: "local" });
    mockListGroups.mockResolvedValue([{ name: "devs", displayName: "Developers", members: ["alice", "bob"] }]);
    renderTab();

    await screen.findByText("Developers");
    expect(screen.getByText("2 members")).toBeInTheDocument();

    await userEvent.click(screen.getByText("+ New Group"));
    await userEvent.type(screen.getByPlaceholderText("developers"), "qa");
    await userEvent.type(screen.getByPlaceholderText("alice, bob, charlie"), "carol, dave");
    await userEvent.click(screen.getByText("Save"));

    await waitFor(() => expect(mockCreateGroup).toHaveBeenCalled());
    expect(mockCreateGroup.mock.calls[0][0]).toMatchObject({ name: "qa", members: ["carol", "dave"] });
  });

  it("local mode: edits members and deletes a group", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    mockUseAuth.mockReturnValue({ authMode: "local" });
    mockListGroups.mockResolvedValue([{ name: "devs", displayName: "Developers", members: ["alice"] }]);
    renderTab();

    await screen.findByText("Developers");

    // Edit members (upsert preserves name, edits member list).
    await userEvent.click(screen.getByText("Edit members"));
    const membersInput = screen.getByDisplayValue("alice");
    await userEvent.clear(membersInput);
    await userEvent.type(membersInput, "alice, bob");
    await userEvent.click(screen.getByText("Save"));
    await waitFor(() => expect(mockCreateGroup).toHaveBeenCalled());
    expect(mockCreateGroup.mock.calls[0][0]).toMatchObject({ name: "devs", members: ["alice", "bob"] });

    // Delete.
    await userEvent.click(screen.getByText("Delete"));
    await waitFor(() => expect(mockDeleteGroup).toHaveBeenCalledWith("devs"));
    vi.unstubAllGlobals();
  });

  it("OIDC mode: read-only group-by, no CRUD controls", async () => {
    mockUseAuth.mockReturnValue({ authMode: "oidc" });
    mockListUsers.mockResolvedValue([
      { name: "alice", groups: ["team-a"], managedBy: "idp" },
      { name: "bob", groups: ["team-a"], managedBy: "idp" },
    ]);
    renderTab();

    await screen.findByText(/groups are observed from identity-provider claims/);
    expect(screen.getByText("team-a")).toBeInTheDocument();
    expect(screen.queryByText("+ New Group")).not.toBeInTheDocument();
    expect(mockListGroups).not.toHaveBeenCalled();
  });

  it("filters groups case-insensitively from the query parameter", async () => {
    mockUseAuth.mockReturnValue({ authMode: "local" });
    mockListGroups.mockResolvedValue([
      { name: "Platform", members: [] },
      { name: "developers", members: [] },
    ]);
    renderTab("/access/groups?query=plat");

    expect(await screen.findByText("Platform")).toBeInTheDocument();
    expect(screen.queryByText("developers")).not.toBeInTheDocument();
  });
});
