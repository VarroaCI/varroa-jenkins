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
  it("renders rbacSpec sub-fields with a human label (Group-to-role bindings), not a JSON blob", () => {
    renderWithQuery(<ControllerSpecForm />);
    // RBACSpec is an object — its sub-field label "Group-to-role bindings"
    // (the ui:title from controllerUiSchema.ts, not the raw JSON name "groups")
    // confirms typed rendering.
    expect(screen.getByText("Group-to-role bindings")).toBeInTheDocument();
  });

  it("does not render excluded fields (version, probes, podOverrides, endpoint)", () => {
    renderWithQuery(<ControllerSpecForm />);
    // These are excluded via EXCLUDED_FROM_TIER1 — their labels should NOT appear.
    // endpoint is the dead field (zero Go readers/writers) excluded in section 5.
    expect(screen.queryByText("version")).not.toBeInTheDocument();
    expect(screen.queryByText("probes")).not.toBeInTheDocument();
    expect(screen.queryByText("podOverrides")).not.toBeInTheDocument();
    expect(screen.queryByText("endpoint")).not.toBeInTheDocument();
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

  it("renders typed fields with a human label (Controller class)", () => {
    renderWithQuery(<ControllerSpecForm />);
    // className's ui:title replaces the raw JSON property name.
    expect(screen.getByText("Controller class")).toBeInTheDocument();
  });

  it("does not render the removed namespace field", () => {
    renderWithQuery(<ControllerSpecForm />);
    expect(screen.queryByText("namespace")).not.toBeInTheDocument();
  });

  it("renders additional-property map rows for a value present at mount (Part 1 regression)", () => {
    // The form mounted WITH the value: RJSF seeds its additional-property row
    // order from the formData it sees at mount, so the rows must render. The
    // companion SpecEditorCard test covers the value-arriving-after-mount
    // (hydration) path, which is the one that was broken.
    renderWithQuery(
      <ControllerSpecForm
        value={{ resources: { limits: { cpu: "500m", memory: "1Gi" } } }}
      />,
    );
    const cpuKey = document.getElementById(
      "root_resources_limits_cpu-key",
    ) as HTMLInputElement | null;
    const memoryKey = document.getElementById(
      "root_resources_limits_memory-key",
    ) as HTMLInputElement | null;
    expect(cpuKey).not.toBeNull();
    expect(memoryKey).not.toBeNull();
    expect(cpuKey?.value).toBe("cpu");
    expect(memoryKey?.value).toBe("memory");
    expect(screen.getByDisplayValue("500m")).toBeInTheDocument();
    expect(screen.getByDisplayValue("1Gi")).toBeInTheDocument();
  });

  it("renders the curated Tier-1 labels in ui:order with the persistence teardown/recreate help", () => {
    renderWithQuery(<ControllerSpecForm />);
    // Every Tier-1 field gets a human label (controllerUiSchema.ts).
    for (const label of [
      "Controller class",
      "Resources",
      "Persistence",
      "RBAC",
      "Plugins",
      "Backups",
    ]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    // The deliberate ui:order puts className first, resources second, persistence third.
    const body = document.body.textContent ?? "";
    expect(body.indexOf("Controller class")).toBeLessThan(body.indexOf("Resources"));
    expect(body.indexOf("Resources")).toBeLessThan(body.indexOf("Persistence"));
    // persistence carries the teardown/recreate caveat (types.go:377-379):
    // volumeClaimTemplates are immutable, so edits apply only after recreate.
    expect(
      screen.getByText(/volumeClaimTemplates are immutable/),
    ).toBeInTheDocument();
    expect(screen.getByText(/teardown\/recreate/)).toBeInTheDocument();
  });
});
