import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { TenantAgentSettings } from "./TenantAgentSettings";
import type { TenantAgent } from "../../types/tenantAgent";

function agent(overrides: Partial<TenantAgent> = {}): TenantAgent {
  return {
    namespace: "varroa-system",
    name: "ta-one",
    tenant: "acme",
    cluster: "core",
    spec: { tenantRef: { name: "acme" }, enabled: true, provider: "smoke", model: "m1", dataSharing: "logs" },
    status: {
      instructionsSHA: "0123456789abcdef",
      conditions: [{ type: "Paused", status: "True", reason: "BudgetDay", lastTransitionTime: "2026-10-18T10:00:00Z" }],
    },
    accepted: true,
    pendingApprovals: 0,
    spendToday: 25,
    dayLimit: 100,
    ...overrides,
  };
}

describe("TenantAgentSettings", () => {
  it("shows the budget against the day limit", () => {
    render(<TenantAgentSettings agent={agent()} />);
    const bar = screen.getByRole("progressbar", { name: "Spend today against the day limit" });
    expect(bar).toHaveAttribute("aria-valuenow", "25");
    expect(bar).toHaveAttribute("aria-valuemax", "100");
    expect(screen.getByText("25 of 100 tokens")).toBeInTheDocument();
  });

  it("lists every condition and the instructions revision", () => {
    render(<TenantAgentSettings agent={agent()} />);
    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(screen.getByText(/BudgetDay/)).toBeInTheDocument();
    expect(screen.getByText("0123456789ab")).toBeInTheDocument();
  });
});
