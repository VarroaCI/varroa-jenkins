import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { RUN_COLUMNS, RunTable } from "./RunTable";

describe("RunTable", () => {
  it("renders the seven run columns when no head is given", () => {
    render(
      <RunTable>
        <tr>
          <td>body</td>
        </tr>
      </RunTable>,
    );
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual([...RUN_COLUMNS]);
    expect(headers).toHaveLength(7);
  });

  it("renders the given head in place of the plain headers", () => {
    render(
      <RunTable head={<th scope="col">Only</th>}>
        <tr>
          <td>body</td>
        </tr>
      </RunTable>,
    );
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["Only"]);
  });
});
