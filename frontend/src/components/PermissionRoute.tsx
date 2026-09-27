import type { ReactNode } from "react";
import { useAuth } from "../context/AuthContext";
import { ApiError } from "../hooks/useApi";
import { canDoAnywhere, canDoGlobal } from "../hooks/usePermissions";
import LoadingSpinner from "./LoadingSpinner";
import { ForbiddenPage, GenericErrorPage } from "./RecoveryState";

export function PermissionRoute({ resource, verb = "read", globalOnly = false, admin = false, children }: { resource?: string; verb?: string; globalOnly?: boolean; admin?: boolean; children: ReactNode }) {
  const { permissions, permissionsLoading, permissionsError, retryPermissions } = useAuth();
  // Before the permissions query answers, every check answers false.
  // A denial before that answer is not a real answer.
  if (permissionsLoading) {
    return (
      <div data-testid="permission-route-loading" role="status" aria-label="Loading permissions">
        <LoadingSpinner />
      </div>
    );
  }
  // A failed read with no earlier answer is not a denial, unless the server answered 403.
  if (permissions === undefined && permissionsError) {
    if (permissionsError instanceof ApiError && permissionsError.status === 403) {
      return <ForbiddenPage />;
    }
    return <GenericErrorPage onRetry={retryPermissions} />;
  }
  const allowed = admin
    ? canDoGlobal(permissions, "*", "*")
    : resource && (globalOnly ? canDoGlobal(permissions, resource, verb) : canDoAnywhere(permissions, resource, verb));
  return allowed ? children : <ForbiddenPage />;
}
