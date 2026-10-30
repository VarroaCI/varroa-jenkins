import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NewPipelinePanel } from "./NewPipelinePanel";

vi.mock("./ProposePipelineCard", () => ({
  default: ({ tenant }: { tenant: string }) => <div>propose for {tenant}</div>,
}));

describe("NewPipelinePanel", () => {
  it("proposes for the first writable tenant and switches with the select", async () => {
    const user = userEvent.setup();
    render(<NewPipelinePanel tenants={["acme", "beta"]} />);
    expect(screen.getByText("propose for acme")).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("Tenant"), "beta");
    expect(screen.getByText("propose for beta")).toBeInTheDocument();
  });

  it("shows no proposal form without a writable tenant", () => {
    render(<NewPipelinePanel tenants={[]} />);
    expect(screen.queryByText(/propose for/)).not.toBeInTheDocument();
    expect(screen.getByText("No tenant grants you write access to pipelines.")).toBeInTheDocument();
  });
});
