import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PermissionRoute } from "./PermissionRoute";

const mockUseAuth = vi.fn();
const mockCanDoAnywhere = vi.fn();
const mockCanDoGlobal = vi.fn();

vi.mock("../context/AuthContext", () => ({ useAuth: () => mockUseAuth() }));
vi.mock("../hooks/usePermissions", () => ({
  canDoAnywhere: (...args: unknown[]) => mockCanDoAnywhere(...args),
  canDoGlobal: (...args: unknown[]) => mockCanDoGlobal(...args),
}));

describe("PermissionRoute", () => {
  beforeEach(() => {
    mockUseAuth.mockReturnValue({ permissions: [] });
    mockCanDoAnywhere.mockReset();
    mockCanDoGlobal.mockReset();
  });

  it("renders children when an anywhere permission is granted", () => {
    mockCanDoAnywhere.mockReturnValue(true);
    render(<PermissionRoute resource="controllers"><span>allowed</span></PermissionRoute>);
    expect(screen.getByText("allowed")).toBeInTheDocument();
  });

  it("checks global permissions for global and admin routes", () => {
    mockCanDoGlobal.mockReturnValue(true);
    const { rerender } = render(<PermissionRoute resource="roles" globalOnly><span>global</span></PermissionRoute>);
    expect(screen.getByText("global")).toBeInTheDocument();
    rerender(<PermissionRoute admin><span>admin</span></PermissionRoute>);
    expect(screen.getByText("admin")).toBeInTheDocument();
  });

  it("renders the forbidden recovery page when permission is denied", () => {
    mockCanDoAnywhere.mockReturnValue(false);
    render(<MemoryRouter><PermissionRoute resource="controllers"><span>hidden</span></PermissionRoute></MemoryRouter>);
    expect(screen.queryByText("hidden")).not.toBeInTheDocument();
    expect(screen.getByText("Access denied")).toBeInTheDocument();
  });
});
