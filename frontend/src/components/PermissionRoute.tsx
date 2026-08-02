import type { ReactNode } from "react";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere, canDoGlobal } from "../hooks/usePermissions";
import { ForbiddenPage } from "./RecoveryState";

export function PermissionRoute({ resource, verb = "read", globalOnly = false, admin = false, children }: { resource?: string; verb?: string; globalOnly?: boolean; admin?: boolean; children: ReactNode }) {
  const { permissions } = useAuth();
  const allowed = admin
    ? canDoGlobal(permissions, "*", "*")
    : resource && (globalOnly ? canDoGlobal(permissions, resource, verb) : canDoAnywhere(permissions, resource, verb));
  return allowed ? children : <ForbiddenPage />;
}
