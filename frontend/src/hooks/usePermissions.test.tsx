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
  canSeePipelines,
  canAccessTenantPipeline,
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

  it("excludes a scopes entry whose tenants list is non-empty", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: [],
          hasControllerSelector: false,
          tenants: [{ name: "team-a" }],
          capabilities: { controllers: { update: true } },
        },
      ],
    };
    expect(canDoAnywhere(perms, "controllers", "update")).toBe(false);
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

  it("excludes a scopes entry whose tenants list is non-empty, even when it covers the namespace", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          tenants: [{ name: "team-a" }],
          capabilities: { controllers: { update: true } },
        },
      ],
    };
    expect(canDoInNamespace(perms, "team-a", "controllers", "update")).toBe(false);
  });
});

describe("canSeePipelines", () => {
  it("returns true when tenants holds an entry", () => {
    const perms = {
      global: {},
      scopes: [],
      tenants: [{ tenant: "payments", jobs: [], capabilities: { pipelines: { read: true } } }],
    };
    expect(canSeePipelines(perms)).toBe(true);
  });

  it("returns true when global grants a pipelines verb", () => {
    const perms = { global: { pipelines: { write: true } }, scopes: [] };
    expect(canSeePipelines(perms)).toBe(true);
  });

  it("returns true when a non-tenant scopes entry grants a pipelines verb", () => {
    const perms = {
      global: {},
      scopes: [
        { namespaces: ["team-a"], hasControllerSelector: false, capabilities: { pipelines: { admin: true } } },
      ],
    };
    expect(canSeePipelines(perms)).toBe(true);
  });

  it("excludes a scopes entry whose tenants list is non-empty", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: [],
          hasControllerSelector: false,
          tenants: [{ name: "payments" }],
          capabilities: { pipelines: { read: true } },
        },
      ],
    };
    expect(canSeePipelines(perms)).toBe(false);
  });

  it("returns false when nothing grants a pipelines verb", () => {
    const perms = { global: { controllers: { read: true } }, scopes: [] };
    expect(canSeePipelines(perms)).toBe(false);
  });

  it("tolerates undefined perms", () => {
    expect(canSeePipelines(undefined)).toBe(false);
  });
});

describe("canAccessTenantPipeline", () => {
  it("admits when one tenant entry holds the verb with no job restriction", () => {
    const perms = {
      global: {},
      scopes: [],
      tenants: [{ tenant: "payments", jobs: [], capabilities: { pipelines: { read: true } } }],
    };
    expect(canAccessTenantPipeline(perms, "payments", "read")).toBe(true);
  });

  it("admits when a job path matches one of the entry's globs", () => {
    const perms = {
      global: {},
      scopes: [],
      tenants: [
        { tenant: "payments", jobs: ["release/*"], capabilities: { pipelines: { read: true } } },
      ],
    };
    expect(canAccessTenantPipeline(perms, "payments", "read", "release/eu")).toBe(true);
    expect(canAccessTenantPipeline(perms, "payments", "read", "build/nightly")).toBe(false);
  });

  it("matches a single Unicode code point outside the Basic Multilingual Plane with a single '?'", () => {
    const perms = {
      global: {},
      scopes: [],
      tenants: [{ tenant: "payments", jobs: ["?"], capabilities: { pipelines: { read: true } } }],
    };
    // U+1F600 is a surrogate pair in UTF-16. A single "?" must match it as
    // one code point, the same unit Go's regexp package matches with ".".
    expect(canAccessTenantPipeline(perms, "payments", "read", "\u{1F600}")).toBe(true);
    // Two code points must not match a pattern that names only one.
    expect(canAccessTenantPipeline(perms, "payments", "read", "\u{1F600}\u{1F600}")).toBe(false);
  });

  it("refuses a Team tenant with no matching entry (no fallback)", () => {
    const perms = {
      global: { pipelines: { read: true } },
      scopes: [],
      tenants: [],
    };
    expect(canAccessTenantPipeline(perms, "payments", "read")).toBe(false);
  });

  it("never unions two entries on one tenant", () => {
    const perms = {
      global: {},
      scopes: [],
      tenants: [
        { tenant: "payments", jobs: ["release/*"], capabilities: { pipelines: { write: true, read: false } } },
        { tenant: "payments", jobs: [], capabilities: { pipelines: { read: true, write: false } } },
      ],
    };
    // The write grant is narrowed to release/*; the read grant carries no
    // write verb. Neither entry alone admits a write outside release/*.
    expect(canAccessTenantPipeline(perms, "payments", "write", "build/nightly")).toBe(false);
    expect(canAccessTenantPipeline(perms, "payments", "write", "release/eu")).toBe(true);
    expect(canAccessTenantPipeline(perms, "payments", "read")).toBe(true);
  });

  it("falls back to global for a synthetic tenant with no entry", () => {
    const perms = { global: { pipelines: { read: true } }, scopes: [], tenants: [] };
    expect(canAccessTenantPipeline(perms, "team-a/my-ctrl", "read")).toBe(true);
  });

  it("falls back to a namespace-covering scope for a synthetic tenant", () => {
    const perms = {
      global: {},
      scopes: [
        { namespaces: ["team-a"], hasControllerSelector: false, capabilities: { pipelines: { read: true } } },
      ],
      tenants: [],
    };
    expect(canAccessTenantPipeline(perms, "team-a/my-ctrl", "read")).toBe(true);
    expect(canAccessTenantPipeline(perms, "team-b/my-ctrl", "read")).toBe(false);
  });

  it("excludes a tenant-scoped scopes entry from the synthetic fallback", () => {
    const perms = {
      global: {},
      scopes: [
        {
          namespaces: ["team-a"],
          hasControllerSelector: false,
          tenants: [{ name: "payments" }],
          capabilities: { pipelines: { read: true } },
        },
      ],
      tenants: [],
    };
    expect(canAccessTenantPipeline(perms, "team-a/my-ctrl", "read")).toBe(false);
  });

  it("tolerates undefined perms", () => {
    expect(canAccessTenantPipeline(undefined, "payments", "read")).toBe(false);
  });
});

// The glob grammar tests below drive jobPatternMatches through
// canAccessTenantPipeline, its only caller, to prove parity with
// internal/rbac/jobglob.Match's grammar: "*" holds no "/", "**" crosses
// "/", "?" matches one code point including "/", a pattern is anchored at
// both ends, and every other character (regex metacharacters included)
// matches only itself.
describe("canAccessTenantPipeline glob grammar", () => {
  function matchesPattern(pattern: string, jobPath: string): boolean {
    const perms = {
      global: {},
      scopes: [],
      tenants: [{ tenant: "payments", jobs: [pattern], capabilities: { pipelines: { read: true } } }],
    };
    return canAccessTenantPipeline(perms, "payments", "read", jobPath);
  }

  it("'*' matches a run of characters that holds no '/'", () => {
    expect(matchesPattern("*", "release")).toBe(true);
    expect(matchesPattern("*", "release/eu")).toBe(false);
  });

  it("'**' matches a run of characters that crosses '/'", () => {
    expect(matchesPattern("**", "release/eu/deploy")).toBe(true);
  });

  it("'?' matches one code point, including '/'", () => {
    expect(matchesPattern("a?b", "a/b")).toBe(true);
    expect(matchesPattern("a?b", "axb")).toBe(true);
    expect(matchesPattern("a?b", "ab")).toBe(false);
  });

  it("a pattern is anchored at both ends", () => {
    expect(matchesPattern("release/*", "xrelease/eu")).toBe(false);
    expect(matchesPattern("release/*", "release/eu/deploy")).toBe(false);
    expect(matchesPattern("release/*", "release/eu")).toBe(true);
  });

  it("regex metacharacters in a pattern match only themselves", () => {
    expect(matchesPattern("a.b", "axb")).toBe(false);
    expect(matchesPattern("a.b", "a.b")).toBe(true);
    expect(matchesPattern("a+b", "aab")).toBe(false);
    expect(matchesPattern("a+b", "a+b")).toBe(true);
  });
});
