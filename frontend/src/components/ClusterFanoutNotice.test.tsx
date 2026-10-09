import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import { ClusterFanoutNotice } from "./ClusterFanoutNotice";
import { renderWithProviders } from "../test/render-utils";

const mockFanoutFailures = vi.fn((): { name: string; ok: boolean; error?: string }[] => []);
vi.mock("../hooks/useControllers", () => ({
  useControllerFanoutFailures: () => ({ data: mockFanoutFailures() }),
}));

describe("ClusterFanoutNotice", () => {
  beforeEach(() => {
    mockFanoutFailures.mockReturnValue([]);
  });

  it("renders nothing when every cluster answered", () => {
    renderWithProviders(<ClusterFanoutNotice />);
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("renders one line per failed cluster", () => {
    mockFanoutFailures.mockReturnValue([
      { name: "member-b", ok: false, error: "cluster unreachable" },
      { name: "hive", ok: false, error: "list too large" },
    ]);
    renderWithProviders(<ClusterFanoutNotice />);
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Controllers of cluster member-b are absent from this list: cluster unreachable.");
    expect(screen.getByRole("status")).toHaveTextContent("Controllers of cluster hive are absent from this list: list too large.");
  });

  it("falls back to a fixed reason when the row has no error text", () => {
    mockFanoutFailures.mockReturnValue([{ name: "member-b", ok: false }]);
    renderWithProviders(<ClusterFanoutNotice />);
    expect(screen.getByText(/the cluster did not answer/)).toBeInTheDocument();
  });
});
