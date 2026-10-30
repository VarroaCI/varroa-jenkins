import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SortableHeader, nextSort } from "./SortableHeader";

type Key = "started" | "tenant";

function Headers({ onSort }: { onSort: (k: Key) => void }) {
  return (
    <table>
      <thead>
        <tr>
          <SortableHeader<Key> label="Started" sortKey="started" activeKey="started" order="desc" onSort={onSort} />
          <SortableHeader<Key> label="Tenant" sortKey="tenant" activeKey="started" order="desc" onSort={onSort} />
        </tr>
      </thead>
    </table>
  );
}

describe("SortableHeader", () => {
  it("marks the active column with aria-sort and the others with none", () => {
    render(<Headers onSort={() => {}} />);
    expect(screen.getByRole("columnheader", { name: /Started/ })).toHaveAttribute("aria-sort", "descending");
    expect(screen.getByRole("columnheader", { name: /Tenant/ })).toHaveAttribute("aria-sort", "none");
  });

  it("calls onSort with its key on click and on Enter", async () => {
    const user = userEvent.setup();
    const onSort = vi.fn();
    render(<Headers onSort={onSort} />);
    await user.click(screen.getByRole("button", { name: /Tenant/ }));
    expect(onSort).toHaveBeenLastCalledWith("tenant");
    screen.getByRole("button", { name: /Started/ }).focus();
    await user.keyboard("{Enter}");
    expect(onSort).toHaveBeenLastCalledWith("started");
  });
});

describe("nextSort", () => {
  it("flips the order when the active key is clicked again", () => {
    expect(nextSort("started", "desc", "started", "desc")).toEqual({ key: "started", order: "asc" });
  });

  it("takes the first order of a new key", () => {
    expect(nextSort("started", "desc", "tenant", "asc")).toEqual({ key: "tenant", order: "asc" });
  });
});
