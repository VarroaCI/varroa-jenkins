import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../hooks/useApi";
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
    mockUseAuth.mockReturnValue({ permissions: {}, permissionsLoading: false });
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

  it("renders a loading state, not a denial, while permissions load", () => {
    mockUseAuth.mockReturnValue({ permissions: undefined, permissionsLoading: true });
    mockCanDoAnywhere.mockReturnValue(false);
    mockCanDoGlobal.mockReturnValue(false);
    render(<MemoryRouter><PermissionRoute resource="fleetlocks" globalOnly><span>locks</span></PermissionRoute></MemoryRouter>);
    expect(screen.queryByText("Access denied")).not.toBeInTheDocument();
    expect(screen.queryByText("locks")).not.toBeInTheDocument();
    expect(screen.getByTestId("permission-route-loading")).toBeInTheDocument();
  });

  it("renders the page once permissions load and allow it", () => {
    mockUseAuth.mockReturnValue({ permissions: { global: { fleetlocks: { read: true } } }, permissionsLoading: false });
    mockCanDoGlobal.mockReturnValue(true);
    render(<MemoryRouter><PermissionRoute resource="fleetlocks" globalOnly><span>locks</span></PermissionRoute></MemoryRouter>);
    expect(screen.getByText("locks")).toBeInTheDocument();
    expect(screen.queryByTestId("permission-route-loading")).not.toBeInTheDocument();
  });

  it("renders the forbidden page once permissions load and deny it", () => {
    mockUseAuth.mockReturnValue({ permissions: { global: {} }, permissionsLoading: false });
    mockCanDoGlobal.mockReturnValue(false);
    render(<MemoryRouter><PermissionRoute resource="fleetlocks" globalOnly><span>locks</span></PermissionRoute></MemoryRouter>);
    expect(screen.queryByText("locks")).not.toBeInTheDocument();
    expect(screen.queryByTestId("permission-route-loading")).not.toBeInTheDocument();
    expect(screen.getByText("Access denied")).toBeInTheDocument();
  });

  it("renders a generic error, not a denial, when the permissions read fails", () => {
    const retry = vi.fn();
    mockUseAuth.mockReturnValue({ permissions: undefined, permissionsLoading: false, permissionsError: new ApiError(502, "bad gateway"), retryPermissions: retry });
    mockCanDoGlobal.mockReturnValue(false);
    render(<MemoryRouter><PermissionRoute resource="fleetlocks" globalOnly><span>locks</span></PermissionRoute></MemoryRouter>);
    expect(screen.queryByText("Access denied")).not.toBeInTheDocument();
    expect(screen.getByText("Unable to load page")).toBeInTheDocument();
    screen.getByRole("button", { name: /retry/i }).click();
    expect(retry).toHaveBeenCalled();
  });

  it("renders the forbidden page when the permissions read answers 403", () => {
    mockUseAuth.mockReturnValue({ permissions: undefined, permissionsLoading: false, permissionsError: new ApiError(403, "forbidden"), retryPermissions: vi.fn() });
    render(<MemoryRouter><PermissionRoute resource="fleetlocks" globalOnly><span>locks</span></PermissionRoute></MemoryRouter>);
    expect(screen.getByText("Access denied")).toBeInTheDocument();
  });

  it("uses the last permissions answer when a later refetch fails", () => {
    mockUseAuth.mockReturnValue({ permissions: { global: { fleetlocks: { read: true } } }, permissionsLoading: false, permissionsError: new ApiError(502, "bad gateway"), retryPermissions: vi.fn() });
    mockCanDoGlobal.mockReturnValue(true);
    render(<MemoryRouter><PermissionRoute resource="fleetlocks" globalOnly><span>locks</span></PermissionRoute></MemoryRouter>);
    expect(screen.getByText("locks")).toBeInTheDocument();
  });
});
