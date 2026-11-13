import type { ObjectMeta } from "../types";

/** MANAGED_BY_LABEL marks an object that a Varroa component generated. */
export const MANAGED_BY_LABEL = "varroa.dev/managed-by";
/** MANAGED_BY_OPERATOR is the managed-by value of a binding that the operator writes. */
export const MANAGED_BY_OPERATOR = "varroa-operator";
/** CONTROLLER_NAMESPACE_LABEL names the namespace of the controller that owns a generated object. */
export const CONTROLLER_NAMESPACE_LABEL = "varroa.dev/controller-namespace";
/** CONTROLLER_NAME_LABEL names the controller that owns a generated object. */
export const CONTROLLER_NAME_LABEL = "varroa.dev/controller-name";

/**
 * RBAC_SPEC_NAME matches the name that the operator derives for an rbacSpec
 * binding: "ctrl-", 16 hex characters, and a role. The roles are the four that
 * the CRD enum of RBACGroupBinding.Role lists. This check does not verify the
 * hash. The server ownership test is the authority.
 */
const RBAC_SPEC_NAME = /^ctrl-[0-9a-f]{16}-(admin|operator|developer|viewer)$/;

/** RbacSpecOwner names the controller whose spec.rbacSpec writes a binding. */
export interface RbacSpecOwner {
  namespace: string;
  name: string;
}

/**
 * rbacSpecOwner returns the controller that writes a binding from its
 * spec.rbacSpec, or null. It needs the operator managed-by label, both
 * controller labels, and a derived name. A copy that keeps the labels under
 * another name is not owned.
 */
export function rbacSpecOwner(metadata?: ObjectMeta): RbacSpecOwner | null {
  if (!metadata) return null;
  const labels = metadata.labels ?? {};
  if (labels[MANAGED_BY_LABEL] !== MANAGED_BY_OPERATOR) return null;
  const namespace = labels[CONTROLLER_NAMESPACE_LABEL];
  const name = labels[CONTROLLER_NAME_LABEL];
  if (!namespace || !name) return null;
  if (!RBAC_SPEC_NAME.test(metadata.name)) return null;
  return { namespace, name };
}

/** rbacSpecOwnerLabel returns the controller as "namespace/name". */
export function rbacSpecOwnerLabel(owner: RbacSpecOwner): string {
  return `${owner.namespace}/${owner.name}`;
}

/** rbacSpecOwnerSummary returns one line that names the owner. A badge shows it as its title. */
export function rbacSpecOwnerSummary(owner: RbacSpecOwner): string {
  return `The operator writes this binding from the rbacSpec of controller ${rbacSpecOwnerLabel(owner)}. Change the rbacSpec of the controller.`;
}
