import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("../api/client", () => ({ getBuiltinRoles: vi.fn() }));

import { getBuiltinRoles } from "../api/client";
import SettingsBuiltinRolesTab from "./SettingsBuiltinRolesTab";

const mockGet = getBuiltinRoles as ReturnType<typeof vi.fn>;

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SettingsBuiltinRolesTab />
    </QueryClientProvider>,
  );
}

beforeEach(() => mockGet.mockReset());

describe("SettingsBuiltinRolesTab", () => {
  it("renders two-plane cards with the Jenkins role deep-link", async () => {
    mockGet.mockResolvedValue([
      {
        name: "varroa:admin",
        apiRules: [{ resources: ["*"], verbs: ["*"] }],
        jenkinsRoleRef: "varroa-admin",
        jenkinsPermissions: ["hudson.model.Hudson.Administer"],
      },
      {
        name: "varroa:viewer",
        apiRules: [{ resources: ["controllers"], verbs: ["read"] }],
        jenkinsRoleRef: "",
        jenkinsPermissions: [],
      },
    ]);
    renderTab();

    await screen.findByText("varroa:admin");
    expect(screen.getByText("varroa:viewer")).toBeInTheDocument();
    // Control-plane verbs + data-plane link.
    expect(screen.getByText("hudson.model.Hudson.Administer")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "varroa-admin" });
    expect(link).toHaveAttribute("href", "/access/jenkins-roles/varroa-admin/edit");
    // Role without a Jenkins ref shows the fallback.
    expect(screen.getByText("No Jenkins role referenced")).toBeInTheDocument();
  });

  it("shows empty state when there are no built-in roles", async () => {
    mockGet.mockResolvedValue([]);
    renderTab();
    await screen.findByText("No built-in roles found.");
  });
});
