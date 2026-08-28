import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

// PluginConflictBanner is a pure presentational component — no hooks, no
// providers needed. Import it directly, mirroring the ReconcileBlockedBanner tests.
import { PluginConflictBanner } from "./ControllerDetail";

describe("PluginConflictBanner", () => {
  it("renders message and reason when pluginConflict is active", () => {
    render(
      <PluginConflictBanner
        pluginConflict={{
          active: true,
          reason: "PluginPinMismatch",
          message: "catalog item 'my-plugin' pin v1.2.3 does not match core lock v1.4.0",
        }}
      />,
    );
    expect(
      screen.getByText(/Plugin lock conflict: catalog item 'my-plugin' pin v1.2.3 does not match core lock v1.4.0/),
    ).toBeInTheDocument();
    expect(screen.getByText(/Reason: PluginPinMismatch/)).toBeInTheDocument();
  });

  it("renders nothing when active is false", () => {
    const { container } = render(
      <PluginConflictBanner
        pluginConflict={{ active: false }}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when pluginConflict is undefined", () => {
    const { container } = render(
      <PluginConflictBanner pluginConflict={undefined} />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
