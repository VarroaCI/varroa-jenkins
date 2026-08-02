import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import TextField from "./TextField";
import NumberField from "./NumberField";
import CheckboxField from "./CheckboxField";
import SelectField from "./SelectField";

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function widgetProps(overrides: Record<string, any> = {}): any {
  return {
    id: "test-field",
    name: "test",
    onChange: vi.fn(),
    onBlur: vi.fn(),
    onFocus: vi.fn(),
    value: "",
    schema: {},
    options: {},
    required: false,
    readonly: false,
    disabled: false,
    autofocus: false,
    label: "Test",
    placeholder: "",
    registry: {
      fields: {},
      widgets: {},
      templates: {},
      rootSchema: {},
      formContext: {},
      schemaUtils: { getSchemaType: () => "string", isValid: () => true },
      translateString: () => "",
      globalFormOptions: {},
    },
    ...overrides,
  };
}

describe("TextField", () => {
  it("renders and fires onChange", async () => {
    const onChange = vi.fn();
    render(<TextField {...widgetProps({ onChange })} />);
    const input = screen.getByRole("textbox");
    await userEvent.type(input, "h");
    expect(onChange).toHaveBeenCalled();
  });
});

describe("NumberField", () => {
  it("renders and fires onChange with a number", async () => {
    const onChange = vi.fn();
    render(<NumberField {...widgetProps({ onChange })} />);
    const input = screen.getByRole("spinbutton");
    await userEvent.type(input, "4");
    expect(onChange).toHaveBeenCalled();
  });
});

describe("CheckboxField", () => {
  it("renders and toggles", async () => {
    const onChange = vi.fn();
    render(<CheckboxField {...widgetProps({ onChange, value: false })} />);
    const cb = screen.getByRole("checkbox");
    await userEvent.click(cb);
    expect(onChange).toHaveBeenCalledWith(true);
  });
});

describe("SelectField", () => {
  it("renders options and fires onChange", async () => {
    const onChange = vi.fn();
    render(
      <SelectField
        {...widgetProps({
          onChange,
          options: {
            enumOptions: [
              { value: "a", label: "A" },
              { value: "b", label: "B" },
            ],
          },
          schema: { enum: ["a", "b"] },
        })}
      />,
    );
    const select = screen.getByRole("combobox");
    await userEvent.selectOptions(select, "a");
    expect(onChange).toHaveBeenCalledWith("a");
  });
});
