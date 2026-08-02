import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import ControllerSpecForm from "./ControllerSpecForm";
import fixture from "../../api/__fixtures__/openapi.json";

// Mock useOpenAPISchema to return fixture data synchronously
vi.mock("../../api/openapiSchema", async () => {
  const actual = await vi.importActual<typeof import("../../api/openapiSchema")>("../../api/openapiSchema");
  return {
    useOpenAPISchema: () => ({
      data: fixture,
      isLoading: false,
      error: null,
    }),
    getControllerSpecSchema: actual.getControllerSpecSchema,
    getPodOverridesSchema: actual.getPodOverridesSchema,
  };
});

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe("ControllerSpecForm", () => {
  it("renders rbacSpec sub-fields (groups) showing typed widgets, not a JSON blob", () => {
    renderWithQuery(<ControllerSpecForm />);
    // RBACSpec is an object — its sub-field label "groups" confirms typed rendering
    expect(screen.getByText("groups")).toBeInTheDocument();
  });

  it("does not render excluded fields (version, probes, podOverrides)", () => {
    renderWithQuery(<ControllerSpecForm />);
    // These are excluded via EXCLUDED_FROM_TIER1 — their labels should NOT appear
    expect(screen.queryByText("version")).not.toBeInTheDocument();
    expect(screen.queryByText("probes")).not.toBeInTheDocument();
    expect(screen.queryByText("podOverrides")).not.toBeInTheDocument();
  });

  it("does not render ingressSpec or miteSpec sub-fields (moved to YAML tiers)", () => {
    renderWithQuery(<ControllerSpecForm />);
    // ingressSpec and miteSpec are excluded via EXCLUDED_FROM_TIER1 — their
    // nested field labels (previously rendered as a broken generated form,
    // see issue #429) should NOT appear.
    expect(screen.queryByText("host")).not.toBeInTheDocument();
    expect(screen.queryByText("annotations")).not.toBeInTheDocument();
    expect(screen.queryByText("imagePullPolicy")).not.toBeInTheDocument();
  });

  it("renders typed fields (className)", () => {
    renderWithQuery(<ControllerSpecForm />);
    expect(screen.getByText("className")).toBeInTheDocument();
  });

  it("renders namespace as read-only with an immutability note", () => {
    const { container } = renderWithQuery(<ControllerSpecForm />);
    expect(screen.getByText("namespace")).toBeInTheDocument();
    const input = container.querySelector("input[readonly]");
    expect(input).not.toBeNull();
    expect(screen.getByText(/immutable after creation/i)).toBeInTheDocument();
  });
});
