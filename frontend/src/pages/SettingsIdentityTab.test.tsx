import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

vi.mock("../api/client", () => ({ getIdentitySettings: vi.fn() }));

import { getIdentitySettings } from "../api/client";
import SettingsIdentityTab from "./SettingsIdentityTab";

const mockGet = getIdentitySettings as ReturnType<typeof vi.fn>;

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter><QueryClientProvider client={qc}>
      <SettingsIdentityTab />
    </QueryClientProvider></MemoryRouter>,
  );
}

beforeEach(() => mockGet.mockReset());

describe("SettingsIdentityTab", () => {
  it("renders OIDC settings read-only (issuer, client id, scopes)", async () => {
    mockGet.mockResolvedValue({
      mode: "oidc",
      cookieDomain: "example.com",
      defaultRead: true,
      issuer: "https://dex.example.com",
      clientId: "varroa",
      scopes: ["openid", "groups"],
    });
    renderTab();

    await screen.findByText("https://dex.example.com");
    expect(screen.getByText("varroa")).toBeInTheDocument();
    expect(screen.getByText("openid, groups")).toBeInTheDocument();
    // No editable controls.
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("renders a local-mode indicator with links to Users and Groups", async () => {
    mockGet.mockResolvedValue({ mode: "local", cookieDomain: "", defaultRead: false });
    renderTab();

    await screen.findByText("Local mode");
    expect(screen.getByRole("link", { name: "Users" })).toHaveAttribute("href", "/access/users");
    expect(screen.getByRole("link", { name: "Groups" })).toHaveAttribute("href", "/access/groups");
    // OIDC-only fields are absent in local mode.
    expect(screen.queryByText("Issuer")).not.toBeInTheDocument();
  });
});
