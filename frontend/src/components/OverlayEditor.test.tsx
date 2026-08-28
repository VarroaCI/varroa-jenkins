import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { OverlayEditor, DiffView, OVERLAY_RESOURCES } from "./OverlayEditor";

const emptyValues = { statefulSet: "", service: "", ingress: "" };

describe("OverlayEditor", () => {
  it("renders a textarea for each overlay resource plus podOverrides", () => {
    render(
      <OverlayEditor
        values={emptyValues}
        onChange={() => {}}
        podOverridesText=""
        onPodOverridesChange={() => {}}
        fieldError={null}
      />,
    );
    for (const { label } of OVERLAY_RESOURCES) {
      expect(screen.getByLabelText(`${label} YAML`)).toBeInTheDocument();
    }
    expect(screen.getByLabelText("podOverrides YAML")).toBeInTheDocument();
  });

  it("calls onChange with the resource key when a textarea is edited", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <OverlayEditor
        values={emptyValues}
        onChange={onChange}
        podOverridesText=""
        onPodOverridesChange={() => {}}
        fieldError={null}
      />,
    );
    await user.type(screen.getByLabelText("Service overlay YAML"), "x");
    expect(onChange).toHaveBeenCalledWith("service", "x");
  });

  it("calls onPodOverridesChange when the podOverrides textarea is edited", async () => {
    const user = userEvent.setup();
    const onPodOverridesChange = vi.fn();
    render(
      <OverlayEditor
        values={emptyValues}
        onChange={() => {}}
        podOverridesText=""
        onPodOverridesChange={onPodOverridesChange}
        fieldError={null}
      />,
    );
    await user.type(screen.getByLabelText("podOverrides YAML"), "x");
    expect(onPodOverridesChange).toHaveBeenCalledWith("x");
  });

  it("surfaces a field error scoped to the offending editor", () => {
    render(
      <OverlayEditor
        values={emptyValues}
        onChange={() => {}}
        podOverridesText=""
        onPodOverridesChange={() => {}}
        fieldError={{ field: "ingress", message: "boom" }}
      />,
    );
    expect(screen.getByText(/boom/)).toBeInTheDocument();
  });

  it("renders warnings when provided", () => {
    render(
      <OverlayEditor
        values={emptyValues}
        onChange={() => {}}
        podOverridesText=""
        onPodOverridesChange={() => {}}
        fieldError={null}
        warnings={[{ resource: "statefulSet", path: "spec.foo", message: "risky" }]}
      />,
    );
    expect(screen.getByText(/risky/)).toBeInTheDocument();
  });
});

describe("DiffView", () => {
  it("renders 'No changes.' for an empty diff", () => {
    render(<DiffView diff="" />);
    expect(screen.getByText("No changes.")).toBeInTheDocument();
  });

  it("renders diff lines", () => {
    render(<DiffView diff={"@@ -1 +1 @@\n-old\n+new"} />);
    expect(screen.getByText("-old")).toBeInTheDocument();
    expect(screen.getByText("+new")).toBeInTheDocument();
  });
});
