import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import {
  usePermissions,
  canDoGlobal,
  canDoAnywhere,
  canDoInNamespace,
} from "./usePermissions";
import { createPermissions } from "../test/factories";

// Mock the API client function used by usePermissions.
const mockGetMyPermissions = vi.fn();

vi.mock("../api/client", () => ({
  getMyPermissions: (...args: unknown[]) => mockGetMyPermissions(...args),
}));

function createWrapper(queryClient = createTestQueryClient()) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  mockGetMyPermissions.mockReset();
});

describe("usePermissions", () => {
  it("is disabled when enabled=false (query not fetched)", () => {
    const { result } = renderHook(() => usePermissions(false), {
      wrapper: createWrapper(),
    });

    expect(result.current.data).toBeUndefined();
    expect(mockGetMyPermissions).not.toHaveBeenCalled();
  });

  it("fetches permissions when enabled=true", async () => {
    const perms = createPermissions();
    mockGetMyPermissions.mockResolvedValueOnce(perms);

    const { result } = renderHook(() => usePermissions(true), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(result.current.data).toEqual(perms);
    expect(mockGetMyPermissions).toHaveBeenCalledTimes(1);
  });

  it("defaults to enabled when called without argument", async () => {
    const perms = createPermissions();
    mockGetMyPermissions.mockResolvedValueOnce(perms);

    const { result } = renderHook(() => usePermissions(), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(result.current.data).toEqual(perms);
    expect(mockGetMyPermissions).toHaveBeenCalled();
  });
});

describe("canDoGlobal", () => {
  it("returns true for wildcard *:*", () => {
    const perms = { global: { "*": { "*": true } }, scopes: [] };
    expect(canDoGlobal(perms, "anything", "anything")).toBe(true);
  });

  it("returns true for *:verb (any resource, specific verb)", () => {
    const perms = { global: { "*": { get: true } }, scopes: [] };
    expect(canDoGlobal(perms, "controllers", "get")).toBe(true);
    expect(canDoGlobal(perms, "controllers", "create")).toBe(false);
  });

  it("returns true for resource:* (specific resource, any verb)", () => {
    const perms = { global: { controllers: { "*": true } }, scopes: [] };
    expect(canDoGlobal(perms, "controllers", "get")).toBe(true);
    expect(canDoGlobal(perms, "controllers", "delete")).toBe(true);
    expect(canDoGlobal(perms, "roles", "get")).toBe(false);
  });

  it("returns true for exact resource:verb match", () => {
    const perms = { global: { controllers: { get: true } }, scopes: [] };
    expect(canDoGlobal(perms, "controllers", "get")).toBe(true);
    expect(canDoGlobal(perms, "controllers", "list")).toBe(false);
  });

  it("returns false when no match exists", () => {
    const perms = { global: { controllers: { get: true } }, scopes: [] };
    expect(canDoGlobal(perms, "roles", "get")).toBe(false);
  });

  it("returns false when perms is undefined", () => {
    expect(canDoGlobal(undefined, "controllers", "get")).toBe(false);
  });

  it("ignores scoped capabilities (global-admin gate)", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { provisioningdefaults: { update: true } },
        },
      ],
    };
    expect(canDoGlobal(perms, "provisioningdefaults", "update")).toBe(false);
  });
});

describe("canDoAnywhere", () => {
  it("returns true from global", () => {
    const perms = { global: { controllers: { read: true } }, scopes: [] };
    expect(canDoAnywhere(perms, "controllers", "read")).toBe(true);
  });

  it("returns true from scopes", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { controllers: { read: true } },
        },
      ],
    };
    expect(canDoAnywhere(perms, "controllers", "read")).toBe(true);
  });

  it("returns false when neither global nor scopes match", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { controllers: { read: true } },
        },
      ],
    };
    expect(canDoAnywhere(perms, "roles", "read")).toBe(false);
  });

  it("tolerates undefined perms", () => {
    expect(canDoAnywhere(undefined, "controllers", "read")).toBe(false);
  });

  it("honors wildcard */* in scoped capabilities", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { "*": { "*": true } },
        },
      ],
    };
    expect(canDoAnywhere(perms, "anything", "anything")).toBe(true);
  });
});

describe("canDoInNamespace", () => {
  it("returns true from global regardless of namespace", () => {
    const perms = { global: { controllers: { read: true } }, scopes: [] };
    expect(canDoInNamespace(perms, "any-ns", "controllers", "read")).toBe(true);
  });

  it("returns true when a scope covers the namespace", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { controllers: { update: true } },
        },
      ],
    };
    expect(canDoInNamespace(perms, "team-a", "controllers", "update")).toBe(true);
  });

  it("returns false when no scope covers the namespace", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { controllers: { update: true } },
        },
      ],
    };
    expect(canDoInNamespace(perms, "team-b", "controllers", "update")).toBe(false);
  });

  it("returns true for a namespace-unbounded scope (no namespaces, no selector)", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: [],
          hasControllerSelector: false,
          capabilities: { controllers: { read: true } },
        },
      ],
    };
    expect(canDoInNamespace(perms, "any-ns", "controllers", "read")).toBe(true);
  });

  it("returns true (permissive) for a selector-only scope", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: [],
          hasControllerSelector: true,
          capabilities: { controllers: { read: true } },
        },
      ],
    };
    expect(canDoInNamespace(perms, "unrelated-ns", "controllers", "read")).toBe(true);
  });

  it("tolerates undefined perms", () => {
    expect(canDoInNamespace(undefined, "ns", "controllers", "read")).toBe(false);
  });

  it("rejects a non-covering namespace when scope is exact", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { controllers: { read: true } },
        },
      ],
    };
    expect(canDoInNamespace(perms, "team-b", "controllers", "read")).toBe(false);
  });

  it("regression: scoped-only operator does NOT pass global gate but DOES pass namespace gate", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          capabilities: { provisioningdefaults: { update: true } },
        },
      ],
    };
    // Settings save (global gate) should be hidden
    expect(canDoGlobal(perms, "provisioningdefaults", "update")).toBe(false);
    // Per-controller action in team-a should be visible
    expect(canDoInNamespace(perms, "team-a", "provisioningdefaults", "update")).toBe(true);
  });
});
