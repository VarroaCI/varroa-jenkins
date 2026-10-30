import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FilterBar, FilterSelect, SearchBox } from "./FilterBar";

const statusOptions = [
  { value: "QUEUED", label: "Queued" },
  { value: "RUNNING", label: "Running" },
];

describe("FilterBar", () => {
  it("labels each select", () => {
    render(
      <FilterBar onClear={() => {}} clearDisabled={false}>
        <FilterSelect id="f-status" label="Status" value="" options={statusOptions} onChange={() => {}} />
      </FilterBar>,
    );
    expect(screen.getByLabelText("Status").tagName).toBe("SELECT");
    expect(screen.getByRole("group", { name: "Filters" })).toBeInTheDocument();
  });

  it("the empty option reads All", () => {
    render(
      <FilterBar onClear={() => {}} clearDisabled={false}>
        <FilterSelect id="f-status" label="Status" value="" options={statusOptions} onChange={() => {}} />
      </FilterBar>,
    );
    expect(screen.getByRole("option", { name: "All" })).toHaveValue("");
  });

  it("reports the chosen value", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <FilterBar onClear={() => {}} clearDisabled={false}>
        <FilterSelect id="f-status" label="Status" value="" options={statusOptions} onChange={onChange} />
      </FilterBar>,
    );
    await user.selectOptions(screen.getByLabelText("Status"), "RUNNING");
    expect(onChange).toHaveBeenCalledWith("RUNNING");
  });

  it("Clear calls onClear and is disabled when clearDisabled", async () => {
    const user = userEvent.setup();
    const onClear = vi.fn();
    const { rerender } = render(
      <FilterBar onClear={onClear} clearDisabled={false}>
        <SearchBox id="f-q" label="Search" value="" onChange={() => {}} />
      </FilterBar>,
    );
    await user.click(screen.getByRole("button", { name: "Clear" }));
    expect(onClear).toHaveBeenCalledTimes(1);
    rerender(
      <FilterBar onClear={onClear} clearDisabled={true}>
        <SearchBox id="f-q" label="Search" value="" onChange={() => {}} />
      </FilterBar>,
    );
    expect(screen.getByRole("button", { name: "Clear" })).toBeDisabled();
  });

  it("SearchBox reports the typed text", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<SearchBox id="f-q" label="Search" value="" onChange={onChange} />);
    await user.type(screen.getByLabelText("Search"), "x");
    expect(onChange).toHaveBeenCalledWith("x");
  });
});
