import { type ReactElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderOptions } from "@testing-library/react";
import { ThemeProvider } from "../context/ThemeContext";
import { AuthProvider } from "../context/AuthContext";
import { ToastProvider } from "../components/Toast";
import { ComposerProvider } from "../context/ComposerContext";
import type { Permissions, MeResponse } from "../types/auth";

interface RenderWithProvidersOptions extends Omit<RenderOptions, "wrapper"> {
  /** Initial route path (default: "/") */
  route?: string;
  /** Override the QueryClient (default: retry: false, no auto-refetch) */
  queryClient?: QueryClient;
  /** Pre-seeded permissions for the auth context */
  permissions?: Permissions;
  /** Auth mode: "local" or "oidc" */
  authMode?: "local" | "oidc";
  /** Pre-seeded user for the auth context */
  user?: MeResponse;
  /** Pre-populate composer context state */
  composerState?: { items: import("../types").ComposedItemRef[]; variables: Record<string, string> };
}

/**
 * Creates a QueryClient suitable for testing: no retries, no refetching.
 */
export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: 0,
        refetchOnWindowFocus: false,
        refetchOnMount: false,
        refetchOnReconnect: false,
        refetchInterval: false,
      },
      mutations: {
        retry: false,
      },
    },
  });
}

/**
 * Renders a React element wrapped in the full provider stack used by the app:
 * MemoryRouter → QueryClientProvider → ThemeProvider → ToastProvider → AuthProvider → ComposerProvider
 *
 * Use this for component and page tests that need the real context chain.
 */
export function renderWithProviders(
  ui: ReactElement,
  options?: RenderWithProvidersOptions,
) {
  const {
    route = "/",
    queryClient = createTestQueryClient(),
    permissions,
    authMode = "local",
    user,
    composerState,
    ...renderOptions
  } = options ?? {};

  // Store test defaults in localStorage so AuthProvider and ThemeProvider pick them up.
  if (user) {
    localStorage.setItem("varroa_user", user.preferredUsername ?? user.name);
  }
  if (authMode) {
    localStorage.setItem("varroa_auth_mode", authMode);
  }
  // Pre-seed permissions in the query cache so AuthProvider doesn't need to fetch.
  if (permissions) {
    queryClient.setQueryData(["permissions"], permissions);
  }
  // Pre-seed composer draft state so ComposerProvider restores it on mount.
  if (composerState) {
    localStorage.setItem("varroa_composer_draft", JSON.stringify(composerState));
  }

  function Wrapper({ children }: { children: React.ReactNode }) {
    return (
      <MemoryRouter initialEntries={[route]}>
        <QueryClientProvider client={queryClient}>
          <ThemeProvider>
            <ToastProvider>
              <AuthProvider>
                <ComposerProvider>{children}</ComposerProvider>
              </AuthProvider>
            </ToastProvider>
          </ThemeProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  }

  const result = render(ui, { wrapper: Wrapper, ...renderOptions });

  return {
    ...result,
    queryClient,
  };
}

export { renderWithProviders as render };
