import type { ObjectMeta } from "../types";

/** FEDERATED_FROM_LABEL marks an object that federation owns. */
export const FEDERATED_FROM_LABEL = "varroa.dev/federated-from";
/** FEDERATED_ROLES_ANNOTATION names the source VarroaRoles of a JenkinsRole copy. */
export const FEDERATED_ROLES_ANNOTATION = "varroa.dev/federated-roles";
/** FEDERATED_BINDINGS_ANNOTATION names the source VarroaRoleBindings of a JenkinsRoleBinding copy. */
export const FEDERATED_BINDINGS_ANNOTATION = "varroa.dev/federated-bindings";
/** FEDERATED_COPY_CODE is the machine code of the 409 that refuses a write to a copy. */
export const FEDERATED_COPY_CODE = "federated_copy";

/** FederatedCopySource names the source objects of a federated copy. */
export interface FederatedCopySource {
  kind: "VarroaRole" | "VarroaRoleBinding";
  names: string[];
}

/**
 * federatedCopySource reports the source objects of a federated copy, or null
 * when the object is not a copy. It mirrors the rule of the BFF refusal: the
 * owner annotation names the sources, and the label names the source
 * VarroaRole when the annotation is absent.
 */
export function federatedCopySource(
  kind: "JenkinsRole" | "JenkinsRoleBinding",
  metadata?: ObjectMeta,
): FederatedCopySource | null {
  const labels = metadata?.labels ?? {};
  const annotations = metadata?.annotations ?? {};
  const from = labels[FEDERATED_FROM_LABEL];
  if (!from) return null;

  let sourceKind: FederatedCopySource["kind"] = "VarroaRole";
  let list = from;
  if (kind === "JenkinsRole" && annotations[FEDERATED_ROLES_ANNOTATION]) {
    list = annotations[FEDERATED_ROLES_ANNOTATION];
  }
  if (kind === "JenkinsRoleBinding" && annotations[FEDERATED_BINDINGS_ANNOTATION]) {
    sourceKind = "VarroaRoleBinding";
    list = annotations[FEDERATED_BINDINGS_ANNOTATION];
  }
  const names = list
    .split(",")
    .map((n) => n.trim())
    .filter((n) => n.length > 0);
  return { kind: sourceKind, names };
}
