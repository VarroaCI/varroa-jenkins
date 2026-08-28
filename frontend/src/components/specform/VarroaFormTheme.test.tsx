import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { customizeValidator } from "@rjsf/validator-ajv8";
import { VarroaForm } from "./VarroaFormTheme";

const validator = customizeValidator();

describe("VarroaFormTheme", () => {
  it("renders a form with string, number, boolean, enum fields", () => {
    const schema = {
      type: "object" as const,
      properties: {
        name: { type: "string" as const, title: "Name" },
        count: { type: "number" as const, title: "Count" },
        active: { type: "boolean" as const, title: "Active" },
        mode: { type: "string" as const, enum: ["a", "b", "c"], title: "Mode" },
      },
    };
    render(
      <VarroaForm schema={schema} validator={validator} />,
    );
    expect(screen.getByText("Name")).toBeInTheDocument();
    expect(screen.getByText("Count")).toBeInTheDocument();
    expect(screen.getByText("Mode")).toBeInTheDocument();
    // Boolean checkbox renders without separate label in RJSF
    expect(screen.getByRole("checkbox")).toBeInTheDocument();
  });
});
