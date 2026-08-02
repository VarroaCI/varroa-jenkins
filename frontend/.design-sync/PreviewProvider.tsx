// Preview-only context wrapper (cfg.provider). Wraps every preview in the app's
// real providers so router/theme/toast/auth-dependent components render. The
// QueryClient is pre-seeded so AuthProvider/usePermissions resolve to realistic
// data instead of failed network fetches (no API in the preview sandbox).
// Lives under .design-sync/ and is exposed on window.Varroa via cfg.extraEntries.
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../src/context/ThemeContext";
import { AuthProvider } from "../src/context/AuthContext";
import { ToastProvider } from "../src/components/Toast";

const client = new QueryClient({
  defaultOptions: {
    queries: { retry: false, staleTime: Infinity, refetchOnWindowFocus: false, refetchInterval: false },
  },
});
client.setQueryData(["auth-config"], { mode: "oidc" });
client.setQueryData(["me"], {
  subject: "u-1027",
  email: "ada@varroa.dev",
  name: "Ada Bramwell",
  displayName: "Ada Bramwell",
  groups: ["varroa-admins"],
  authMode: "oidc",
});
client.setQueryData(["permissions"], { "*": { "*": true } });

export function PreviewProvider({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/controllers"]}>
        <ThemeProvider>
          <ToastProvider>
            <AuthProvider>{children}</AuthProvider>
          </ToastProvider>
        </ThemeProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
}
