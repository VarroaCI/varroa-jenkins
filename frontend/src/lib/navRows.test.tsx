import type { ReactElement } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, fireEvent, cleanup } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { Sidebar } from "../components/Sidebar";
import { TabBar } from "../components/TabBar";
import { NAV_GROUPS, PROFILE_ROW, TAB_ROWS, navRowVisible, visibleNavGroups, type NavRow } from "./navRows";
import type { Permissions } from "../types/auth";

vi.mock("../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../hooks/usePermissions")>(
    "../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});

vi.mock("../hooks/useControllers", () => ({
  useControllers: vi.fn(),
}));

import { usePermissions } from "../hooks/usePermissions";
import { useControllers } from "../hooks/useControllers";

function mockPermissionsData(data: Permissions | undefined) {
  vi.mocked(usePermissions).mockReturnValue({
    data,
    isSuccess: data !== undefined,
    isPending: data === undefined,
  } as unknown as ReturnType<typeof usePermissions>);
}

function renderIn(ui: ReactElement) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={createTestQueryClient()}>{ui}</QueryClientProvider>
    </MemoryRouter>,
  );
}

// renderSurfaces renders the sidebar, then the tab bar with its More sheet
// open, and returns the three surfaces.
function renderSurfaces() {
  const { container } = renderIn(<Sidebar />);
  const sidebar = container.querySelector("aside") as HTMLElement;
  const sidebarHrefs = hrefs(sidebar);
  const sidebarHasRow = (row: NavRow) => linkFor(sidebar, row) !== null;
  const sidebarRows = [...ALL_ROWS, PROFILE_ROW].filter(sidebarHasRow);
  const sidebarLabels = NAV_GROUPS.map((group) => group.label).filter((label) => within(sidebar).queryByText(label) !== null);
  cleanup();

  renderIn(<TabBar />);
  fireEvent.click(screen.getByRole("button", { name: "More" }));
  const bar = screen.getByRole("navigation", { name: "Primary" });
  const sheet = screen.getByRole("dialog", { name: "More navigation" });
  return { sidebarHrefs, sidebarRows, sidebarLabels, bar, sheet };
}

// linkFor finds the link of one row inside one surface. The label check keeps
// the sidebar brand link to "/" from standing in for the Overview row.
function linkFor(surface: HTMLElement, row: NavRow): HTMLElement | null {
  return within(surface).queryAllByRole("link").find(
    (link) => link.getAttribute("href") === row.to && (link.textContent ?? "").includes(row.label),
  ) ?? null;
}

function hrefs(surface: HTMLElement): (string | null)[] {
  return within(surface).getAllByRole("link").map((link) => link.getAttribute("href"));
}

const ALL_ROWS: NavRow[] = NAV_GROUPS.flatMap((group) => [...group.rows]);
const SHEET_ROWS: NavRow[] = ALL_ROWS.filter((row) => !row.tab);

const PERMISSION_SETS: [string, Permissions | undefined][] = [
  ["a full-permission caller", { global: { "*": { "*": true } }, scopes: [] }],
  ["a caller with no grant", { global: {}, scopes: [] }],
  ["an unresolved permissions read", undefined],
  ["a pipelines-only caller", { global: {}, scopes: [], tenants: [{ tenant: "acme", jobs: [], capabilities: { pipelines: { read: true } } }] }],
  ["a namespace-scoped controllers:read grant", { global: {}, scopes: [{ namespaces: ["ns-a"], hasControllerSelector: false, capabilities: { controllers: { read: true } } }] }],
  ["a tenant-scoped controllers:read grant", { global: {}, scopes: [{ namespaces: [], hasControllerSelector: false, tenants: [{ name: "acme" }], capabilities: { controllers: { read: true } } }] }],
  ["a cluster-wide cellpools:read grant", { global: { cellpools: { read: true } }, scopes: [] }],
  ["a namespace-scoped cellpools:read grant", { global: {}, scopes: [{ namespaces: ["ns-a"], hasControllerSelector: false, capabilities: { cellpools: { read: true } } }] }],
  ["a cluster-wide agentsessions:read grant", { global: { agentsessions: { read: true } }, scopes: [] }],
  ["a namespace-scoped agentsessions:read grant", { global: {}, scopes: [{ namespaces: ["ns-a"], hasControllerSelector: false, capabilities: { agentsessions: { read: true } } }] }],
  ["a namespace-scoped catalogitems:read grant", { global: {}, scopes: [{ namespaces: ["ns-a"], hasControllerSelector: false, capabilities: { catalogitems: { read: true } } }] }],
  ["a provisioningdefaults:update grant", { global: { provisioningdefaults: { update: true } }, scopes: [] }],
];

describe("navigation row table", () => {
  beforeEach(() => {
    vi.mocked(useControllers).mockReturnValue({ data: undefined } as unknown as ReturnType<typeof useControllers>);
  });

  it("keeps the tab bar within its four link slots", () => {
    expect(TAB_ROWS.length).toBeLessThanOrEqual(4);
  });

  // This test fails when a table row reaches neither the sidebar nor a phone
  // surface, or when it reaches both phone surfaces.
  it("sends every row to the sidebar and to exactly one phone surface for a full-permission caller", () => {
    const full: Permissions = { global: { "*": { "*": true } }, scopes: [] };
    mockPermissionsData(full);
    for (const row of [...ALL_ROWS, PROFILE_ROW]) {
      expect(navRowVisible(row, full), row.label).toBe(true);
    }

    const { sidebarRows, bar, sheet } = renderSurfaces();
    expect(sidebarRows.map((row) => row.to)).toEqual([...ALL_ROWS, PROFILE_ROW].map((row) => row.to));
    for (const row of ALL_ROWS) {
      expect(linkFor(bar, row) !== null, `tab bar ${row.label}`).toBe(row.tab === true);
      expect(linkFor(sheet, row) !== null, `More sheet ${row.label}`).toBe(row.tab !== true);
    }
    expect(linkFor(sheet, PROFILE_ROW)).not.toBeNull();
  });

  it.each(PERMISSION_SETS)("shows the same rows in the same order on the sidebar and on the phone for %s", (_name, perms) => {
    mockPermissionsData(perms);
    const visible = (row: NavRow) => navRowVisible(row, perms);
    const { sidebarHrefs, sidebarLabels, bar, sheet } = renderSurfaces();

    // The sidebar: the brand link, the visible rows in table order, then the foot row.
    expect(sidebarHrefs).toEqual(["/", ...ALL_ROWS.filter(visible).map((row) => row.to), PROFILE_ROW.to]);
    // The sidebar shows a group label only when the group has a visible row.
    expect(sidebarLabels).toEqual(NAV_GROUPS.filter((group) => group.rows.some(visible)).map((group) => group.label));
    // The phone: the tab bar and the sheet split the same visible rows, in table order.
    expect(hrefs(bar)).toEqual(TAB_ROWS.filter(visible).map((row) => row.to));
    expect(hrefs(sheet)).toEqual([...SHEET_ROWS.filter(visible).map((row) => row.to), PROFILE_ROW.to]);
  });
});

describe("visibleNavGroups", () => {
  const labels = (perms: Permissions | undefined) => visibleNavGroups(NAV_GROUPS, perms).map((group) => group.label);

  it("keeps every group and every row for a full-permission caller", () => {
    const full: Permissions = { global: { "*": { "*": true } }, scopes: [] };
    expect(visibleNavGroups(NAV_GROUPS, full)).toEqual(NAV_GROUPS.map((group) => ({ label: group.label, rows: [...group.rows] })));
  });

  it("leaves out the Manage group for a caller with neither door", () => {
    expect(labels({ global: { controllers: { read: true } }, scopes: [] })).toEqual(["Operate", "Brood"]);
  });

  it("leaves out the Manage group while the permissions read has not answered", () => {
    expect(labels(undefined)).toEqual(["Operate", "Brood"]);
  });

  it("keeps a group for the grant of one row alone, with that row only", () => {
    const groups = visibleNavGroups(NAV_GROUPS, { global: { provisioningdefaults: { update: true } }, scopes: [] });
    expect(groups.map((group) => group.label)).toEqual(["Operate", "Brood", "Manage"]);
    expect(groups[2].rows.map((row) => row.label)).toEqual(["Admin & access"]);
  });
});
