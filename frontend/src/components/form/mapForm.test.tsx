import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { customizeValidator } from "@rjsf/validator-ajv8";
import { ADDITIONAL_PROPERTY_FLAG } from "@rjsf/utils";
import { VarroaForm } from "../specform/VarroaFormTheme";
import MapEntry from "./MapEntry";

const validator = customizeValidator();

const mapSchema = {
  type: "object" as const,
  properties: {
    limits: {
      type: "object" as const,
      additionalProperties: { type: "string" as const },
    },
  },
};

function keyInput(container: HTMLElement, key: string): HTMLInputElement {
  const input = container.querySelector<HTMLInputElement>(`#root_limits_${key}-key`);
  if (!input) throw new Error(`no key input for ${key}`);
  return input;
}

describe("map form rendering (ObjectGroup + MapEntry)", () => {
  it("3.1 renders additional-property rows for a map with no fixed properties", () => {
    const { container } = render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: { cpu: "500m", memory: "1Gi" } }}
        validator={validator}
      />,
    );
    expect(keyInput(container, "cpu").value).toBe("cpu");
    expect(keyInput(container, "memory").value).toBe("memory");
    // Value widgets render alongside
    expect(screen.getByDisplayValue("500m")).toBeInTheDocument();
    expect(screen.getByDisplayValue("1Gi")).toBeInTheDocument();
  });

  it("3.2 offers the Add control for an empty expandable map", () => {
    render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: {} }}
        validator={validator}
      />,
    );
    expect(screen.getByRole("button", { name: "+ Add" })).toBeInTheDocument();
  });

  it("3.2/7.9: a readonly map renders no Add, no Remove, and a non-editable key input", () => {
    const { container } = render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: { cpu: "500m" } }}
        readonly
        validator={validator}
      />,
    );
    expect(screen.queryByRole("button", { name: "+ Add" })).not.toBeInTheDocument();
    // design §8: readonly is a pure view — the Remove control is absent too.
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    // The key input is read-only (non-editable)…
    const key = keyInput(container, "cpu");
    expect(key).toHaveAttribute("readonly");
    // …and the value widget is non-editable too (RJSF renders readonly widgets
    // as read-only inputs).
    const value = container.querySelector<HTMLInputElement>("#root_limits_cpu");
    expect(value).not.toBeNull();
    expect(value).toHaveAttribute("readonly");
  });

  it("3.2 suppresses the Add control when the map is disabled", () => {
    render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: { cpu: "500m" } }}
        disabled
        validator={validator}
      />,
    );
    expect(screen.queryByRole("button", { name: "+ Add" })).not.toBeInTheDocument();
  });

  it("rejects a duplicate key via the map key context (no global state)", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: { cpu: "500m", memory: "1Gi" } }}
        validator={validator}
      />,
    );
    const memoryKey = keyInput(container, "memory");
    await user.clear(memoryKey);
    await user.type(memoryKey, "cpu");
    await user.tab(); // blur commits the rename
    expect(memoryKey.value).toBe("memory"); // reverted, not renamed
    expect(screen.getByText('"cpu" is already set')).toBeInTheDocument();
  });

  it("accepts a valid rename to a key not in the map", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: { cpu: "500m", memory: "1Gi" } }}
        validator={validator}
      />,
    );
    const memoryKey = keyInput(container, "memory");
    await user.clear(memoryKey);
    await user.type(memoryKey, "ephemeral-storage");
    await user.tab();
    expect(memoryKey.value).toBe("ephemeral-storage");
  });

  it("scopes the sibling set to the current map only", async () => {
    // Two independent forms on the same page must not share a sibling set.
    const user = userEvent.setup();
    const { container } = render(
      <div>
        <VarroaForm
          schema={mapSchema}
          formData={{ limits: { cpu: "500m", memory: "1Gi" } }}
          validator={validator}
        />
        <VarroaForm
          schema={mapSchema}
          formData={{ limits: { disks: "100M" } }}
          validator={validator}
        />
      </div>,
    );
    // Renaming the second map's "disks" key to "cpu" must succeed — "cpu" only
    // exists in the FIRST map.
    const disksKey = container.querySelector<HTMLInputElement>("#root_limits_disks-key");
    if (!disksKey) throw new Error("no disks key input");
    await user.clear(disksKey);
    await user.type(disksKey, "cpu");
    await user.tab();
    expect(disksKey.value).toBe("cpu");
  });

  it("3.3 moves focus to the new row's key input after an add", async () => {
    const user = userEvent.setup();
    render(
      <VarroaForm
        schema={mapSchema}
        formData={{ limits: {} }}
        validator={validator}
      />,
    );
    await user.click(screen.getByRole("button", { name: "+ Add" }));
    const newKeyInput = document.getElementById("root_limits_newKey-key");
    expect(newKeyInput).not.toBeNull();
    expect(document.activeElement).toBe(newKeyInput);
  });

  it("3.4 renders a datalist from ui:options.keySuggestions without restricting input", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <VarroaForm
        schema={mapSchema}
        uiSchema={{
          limits: {
            "ui:options": { keySuggestions: ["cpu", "memory", "ephemeral-storage"] },
          },
        }}
        formData={{ limits: {} }}
        validator={validator}
      />,
    );
    await user.click(screen.getByRole("button", { name: "+ Add" }));
    const newKeyInput = keyInput(container, "newKey");
    // Datalist is wired up
    const datalist = document.getElementById("root_limits_newKey-key-suggestions");
    expect(datalist).not.toBeNull();
    const options = (datalist as HTMLElement).querySelectorAll("option");
    expect(Array.from(options, (o) => o.getAttribute("value"))).toEqual([
      "cpu",
      "memory",
      "ephemeral-storage",
    ]);
    // The datalist must not restrict input: an arbitrary key is typable.
    await user.clear(newKeyInput);
    await user.type(newKeyInput, "hugepages-2Mi");
    await user.tab();
    expect(newKeyInput.value).toBe("hugepages-2Mi");
  });
});

describe("MapEntry defensive behavior", () => {
  const additionalSchema = {
    [ADDITIONAL_PROPERTY_FLAG as unknown as string]: true,
    type: "object" as const,
  };

  const baseProps = {
    id: "root_limits_cpu",
    label: "cpu",
    displayLabel: true,
    readonly: false,
    disabled: false,
    schema: additionalSchema,
    uiSchema: {},
    onKeyRename: () => {},
    onKeyRenameBlur: () => {},
    onRemoveProperty: () => {},
    registry: {
      fields: {},
      widgets: {},
      templates: {},
      rootSchema: {},
      formContext: {},
      schemaUtils: { getSchemaType: () => "object", isValid: () => true },
      translateString: () => "Key for cpu",
      globalFormOptions: { idPrefix: "root", idSeparator: "_" },
      // Only the parts MapEntry reads are exercised by the no-provider test.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  };

  it("D: renders without throwing when no provider is above", () => {
    const { container } = render(
      <MapEntry {...baseProps} children={<input aria-label="value" />} />,
    );
    expect(container.querySelector("#root_limits_cpu-key")).not.toBeNull();
  });
});
