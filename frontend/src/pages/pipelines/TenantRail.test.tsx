import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { PipelinesOverviewTenant } from "../../types";
import { TenantRail } from "./TenantRail";

function tenant(over: Partial<PipelinesOverviewTenant> = {}): PipelinesOverviewTenant {
  return {
    name: "pay",
    clusters: ["core"],
    placement: "placed",
    runs24h: 4,
    failed24h: 0,
    running: 0,
    waitingInput: 0,
    locksHeld: 0,
    recent: [],
    ...over,
  };
}

function renderRail(
  tenants: PipelinesOverviewTenant[],
  props: { selected?: string[]; onSelect?: (names: string[]) => void } = {},
) {
  const onSelect = props.onSelect ?? vi.fn();
  render(
    <MemoryRouter>
      <TenantRail tenants={tenants} localCluster="core" selected={props.selected ?? []} onSelect={onSelect} />
    </MemoryRouter>,
  );
  return onSelect;
}

describe("TenantRail", () => {
  it("keeps the BFF order", () => {
    renderRail([tenant({ name: "ops" }), tenant({ name: "pay" }), tenant({ name: "idle", runs24h: 0 })]);
    const names = screen
      .getAllByRole("button", { pressed: false })
      .map((b) => b.textContent ?? "")
      .filter((t) => t !== "All tenants" && t !== "");
    expect(names[0]).toContain("ops");
    expect(names[1]).toContain("pay");
    expect(names[2]).toContain("idle");
  });

  it("marks the selected tenant", () => {
    renderRail([tenant({ name: "ops" }), tenant({ name: "pay" })], { selected: ["pay"] });
    expect(screen.getByRole("button", { name: /^pay/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /^ops/ })).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: "All tenants" })).toHaveAttribute("aria-pressed", "false");
  });

  it("strip pads to ten cells", () => {
    renderRail([
      tenant({
        recent: [
          { runId: "a", status: "COMPLETED", result: "SUCCESS" },
          { runId: "b", status: "COMPLETED", result: "FAILURE" },
          { runId: "c", status: "RUNNING" },
        ],
      }),
    ]);
    const row = screen.getByRole("button", { name: /^pay/ }).closest("li") as HTMLElement;
    expect(within(row).getAllByRole("img")).toHaveLength(10);
    expect(within(row).getByLabelText("Failed")).toBeInTheDocument();
    expect(within(row).getByLabelText("Running")).toBeInTheDocument();
  });

  it("meta line names a member cluster", () => {
    renderRail([tenant({ name: "shop", clusters: ["core", "edge"], runs24h: 9, running: 2 })]);
    const row = screen.getByRole("button", { name: /^shop/ }).closest("li") as HTMLElement;
    expect(within(row).getByText(/9 runs · 2 running/)).toBeInTheDocument();
    expect(within(row).getByText(/edge/)).toBeInTheDocument();
  });

  it("meta line says no runs when the window holds none", () => {
    renderRail([tenant({ name: "idle", runs24h: 0 })]);
    expect(screen.getByText("no runs in 24 h")).toBeInTheDocument();
  });

  it("meta line says waits for input", () => {
    renderRail([tenant({ name: "ops", waitingInput: 1 })]);
    expect(screen.getByText(/waits for input/)).toBeInTheDocument();
  });

  it("All tenants clears the selection", async () => {
    const user = userEvent.setup();
    const onSelect = renderRail([tenant({ name: "pay" })], { selected: ["pay"] });
    await user.click(screen.getByRole("button", { name: "All tenants" }));
    expect(onSelect).toHaveBeenCalledWith([]);
  });

  it("omits All tenants when one tenant is the whole view", () => {
    const onSelect = vi.fn();
    render(
      <MemoryRouter>
        <TenantRail tenants={[tenant({ name: "pay" })]} localCluster="core" selected={["pay"]} onSelect={onSelect} showAll={false} />
      </MemoryRouter>,
    );
    expect(screen.queryByRole("button", { name: "All tenants" })).not.toBeInTheDocument();
  });

  it("a row click selects that tenant", async () => {
    const user = userEvent.setup();
    const onSelect = renderRail([tenant({ name: "pay" })]);
    await user.click(screen.getByRole("button", { name: /^pay/ }));
    expect(onSelect).toHaveBeenCalledWith(["pay"]);
  });

  it("the Open link names the build list of the tenant", () => {
    renderRail([tenant({ name: "pay" })]);
    expect(screen.getByRole("link", { name: "Open pay" })).toHaveAttribute("href", "/pipelines/pay");
  });
});
