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

  it("renders the Ingress and Mite sidecar groups with human labels", () => {
    renderWithQuery(<ControllerSpecForm />);
    expect(screen.getByText("Ingress")).toBeInTheDocument();
    expect(screen.getByText("Mite sidecar")).toBeInTheDocument();
    expect(screen.getByText("Routing mode")).toBeInTheDocument();
    expect(screen.getByText("Mite limits")).toBeInTheDocument();
  });

  it("renders the ingress routing mode read-only with its help text", () => {
    renderWithQuery(<ControllerSpecForm value={{ ingressSpec: { mode: "path" } }} />);
    const mode = document.getElementById("root_ingressSpec_mode") as HTMLInputElement;
    expect(mode).toBeDisabled();
    expect(screen.getByText(/routing mode cannot change after the controller is created/)).toBeInTheDocument();
  });

  it("renders cell.enabled read-only with its help text", () => {
    renderWithQuery(<ControllerSpecForm value={{ cell: { enabled: true, maxTenants: 5 } }} />);
    const enabled = document.getElementById("root_cell_enabled") as HTMLInputElement;
    expect(enabled).toBeDisabled();
    expect(enabled).toBeChecked();
    expect(screen.getByText(/cell role cannot change after the controller is created/)).toBeInTheDocument();
  });

  it("keeps cell.maxTenants editable", () => {
    renderWithQuery(<ControllerSpecForm value={{ cell: { enabled: true, maxTenants: 5 } }} />);
    expect(document.getElementById("root_cell_maxTenants")).not.toBeDisabled();
  });

  it("renders cell.enabled read-only when the controller has no cell block", () => {
    renderWithQuery(<ControllerSpecForm />);
    const enabled = document.getElementById("root_cell_enabled") as HTMLInputElement;
    expect(enabled).toBeDisabled();
    expect(enabled).not.toBeChecked();
  });

  it("renders ingress annotation rows whose keys contain a prefix and a slash", () => {
    renderWithQuery(
      <ControllerSpecForm
        value={{ ingressSpec: { annotations: { "nginx.ingress.kubernetes.io/proxy-body-size": "512m" } } }}
      />,
    );
    const keyInput = document.getElementById(
      "root_ingressSpec_annotations_nginx.ingress.kubernetes.io/proxy-body-size-key",
    ) as HTMLInputElement | null;
    expect(keyInput).not.toBeNull();
    expect(keyInput?.value).toBe("nginx.ingress.kubernetes.io/proxy-body-size");
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
