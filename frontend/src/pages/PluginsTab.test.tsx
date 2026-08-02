import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import PluginsTab from "./PluginsTab";
import type { PluginInventorySummary } from "../hooks/useControllers";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
});

function withPI(pluginInventory?: PluginInventorySummary) {
  return { pluginInventory } as { pluginInventory?: PluginInventorySummary };
}

describe("PluginsTab", () => {
  it("shows empty state when pluginInventory is absent", () => {
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <PluginsTab ctrl={withPI()} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText(/No plugin inventory available/i)).toBeDefined();
  });

  it("renders counts from the inventory", () => {
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <PluginsTab
            ctrl={withPI({
              hash: "v1:test",
              source: "jenkins-api",
              stale: false,
              degraded: false,
              bootstrapApproximate: false,
              optionalEdgesDropped: false,
              truncated: false,
              total: 84,
              driftTruncated: false,
              counts: { declared: 76, bootstrap: 1, dependency: 4, "optional-dependency": 1, unmanaged: 2 },
            })}
          />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText(/Bootstrap/)).toBeDefined();
    expect(screen.getByText(/Declared/)).toBeDefined();
    expect(screen.getByText(/Unmanaged/)).toBeDefined();
  });

  it("shows stale banner when stale", () => {
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <PluginsTab
            ctrl={withPI({
              hash: "v1:test",
              source: "jenkins-api",
              stale: true,
              degraded: false,
              bootstrapApproximate: false,
              optionalEdgesDropped: false,
              truncated: false,
              total: 0,
              driftTruncated: false,
            })}
          />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText(/Stale/)).toBeDefined();
    expect(screen.getByText(/Inventory is stale/)).toBeDefined();
  });

  it("shows degraded banner when degraded", () => {
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <PluginsTab
            ctrl={withPI({
              hash: "v1:test",
              source: "filesystem",
              stale: false,
              degraded: true,
              bootstrapApproximate: false,
              optionalEdgesDropped: false,
              truncated: false,
              total: 10,
              driftTruncated: false,
            })}
          />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText(/Degraded/)).toBeDefined();
    expect(screen.getByText(/filesystem/)).toBeDefined();
  });

  it("has no remediation controls — no adopt, remove, or enforce actions", () => {
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <PluginsTab
            ctrl={withPI({
              hash: "v1:test",
              source: "jenkins-api",
              stale: false,
              degraded: false,
              bootstrapApproximate: false,
              optionalEdgesDropped: false,
              truncated: false,
              total: 84,
              driftTruncated: false,
              drift: [{ name: "rogue", version: "1.0", class: "unmanaged" }],
            })}
          />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const buttons = screen.queryAllByRole("button");
    expect(buttons).toHaveLength(0);

    const links = screen.queryAllByRole("link");
    expect(links).toHaveLength(0);

    expect(screen.queryByText(/adopt/i)).toBeNull();
    expect(screen.queryByText(/remove/i)).toBeNull();
    expect(screen.queryByText(/enforce/i)).toBeNull();
  });
});
