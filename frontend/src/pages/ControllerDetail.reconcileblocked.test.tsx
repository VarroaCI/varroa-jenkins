import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

// ReconcileBlockedBanner is a pure presentational component — no hooks, no
// providers needed. Import it directly, mirroring the VersionRollBanner tests.
import { ReconcileBlockedBanner } from "./ControllerDetail";

describe("ReconcileBlockedBanner", () => {
  it("renders reason and message when blocked", () => {
    render(
      <ReconcileBlockedBanner
        reconcileBlocked={{
          blocked: true,
          reason: "BundleUnreadable",
          message: "spec.composedBundleRef is required",
          since: "2025-01-15T10:30:00Z",
        }}
      />,
    );
    expect(
      screen.getByText(/Reconcile blocked: spec.composedBundleRef is required/),
    ).toBeInTheDocument();
    expect(screen.getByText(/Reason: BundleUnreadable/)).toBeInTheDocument();
    expect(screen.getByText(/Since: 2025-01-15T10:30:00Z/)).toBeInTheDocument();
  });

  it("renders nothing when blocked is false", () => {
    const { container } = render(
      <ReconcileBlockedBanner
        reconcileBlocked={{ blocked: false }}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when reconcileBlocked is undefined", () => {
    const { container } = render(
      <ReconcileBlockedBanner reconcileBlocked={undefined} />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
