import type { ObjectMeta, VarroaRoleBindingSpec } from "../types";

/** TENANT_AGENT_LABEL marks a binding that the operator writes for a TenantAgent. */
export const TENANT_AGENT_LABEL = "varroa.dev/tenant-agent";
/** TENANT_AGENT_NAMESPACE_LABEL names the namespace of the TenantAgent. */
export const TENANT_AGENT_NAMESPACE_LABEL = "varroa.dev/tenant-agent-namespace";
/** TENANT_AGENT_NAME_LABEL names the TenantAgent. */
export const TENANT_AGENT_NAME_LABEL = "varroa.dev/tenant-agent-name";
/** BROOD_SCHEDULE_NAMESPACE_ANNOTATION names the namespace of the BroodSchedule. */
export const BROOD_SCHEDULE_NAMESPACE_ANNOTATION = "varroa.dev/broodschedule-namespace";
/** BROOD_SCHEDULE_NAME_ANNOTATION names the BroodSchedule. */
export const BROOD_SCHEDULE_NAME_ANNOTATION = "varroa.dev/broodschedule-name";

/**
 * TENANT_AGENT_BINDING_NAME matches the name that the operator derives for a
 * TenantAgent binding: "tenantagent-" and 16 hex characters. This check does
 * not verify the hash. The server rule v1alpha1.VarroaRoleBindingOwner is the
 * authority.
 */
const TENANT_AGENT_BINDING_NAME = /^tenantagent-[0-9a-f]{16}$/;

/**
 * BROOD_SCHEDULE_BINDING_NAME matches the name that the operator derives for a
 * BroodSchedule binding: "broodschedule-" and 48 hex characters. This check
 * does not verify the hash. The server rule v1alpha1.VarroaRoleBindingOwner is
 * the authority.
 */
const BROOD_SCHEDULE_BINDING_NAME = /^broodschedule-[0-9a-f]{48}$/;

/** RoleBindingOwnerKind is the kind of object whose reconciler writes a binding. */
export type RoleBindingOwnerKind = "TenantAgent" | "BroodSchedule";

/** RoleBindingOwner names the object whose reconciler writes a VarroaRoleBinding. */
export interface RoleBindingOwner {
  kind: RoleBindingOwnerKind;
  namespace: string;
  name: string;
}

/**
 * roleBindingOwner returns the TenantAgent or the BroodSchedule that writes a
 * binding, or null. A TenantAgent needs its three labels and a derived name. A
 * BroodSchedule needs its two annotations and a derived name. A copy that keeps
 * the labels or the annotations under another name is not owned.
 */
export function roleBindingOwner(metadata?: ObjectMeta): RoleBindingOwner | null {
  if (!metadata) return null;
  const labels = metadata.labels ?? {};
  const annotations = metadata.annotations ?? {};
  const taNamespace = labels[TENANT_AGENT_NAMESPACE_LABEL];
  const taName = labels[TENANT_AGENT_NAME_LABEL];
  if (labels[TENANT_AGENT_LABEL] === "true" && taNamespace && taName && TENANT_AGENT_BINDING_NAME.test(metadata.name)) {
    return { kind: "TenantAgent", namespace: taNamespace, name: taName };
  }
  const bsNamespace = annotations[BROOD_SCHEDULE_NAMESPACE_ANNOTATION];
  const bsName = annotations[BROOD_SCHEDULE_NAME_ANNOTATION];
  if (bsNamespace && bsName && BROOD_SCHEDULE_BINDING_NAME.test(metadata.name)) {
    return { kind: "BroodSchedule", namespace: bsNamespace, name: bsName };
  }
  return null;
}

/** roleBindingOwnerRef returns the owner as "namespace/name". */
export function roleBindingOwnerRef(owner: RoleBindingOwner): string {
  return `${owner.namespace}/${owner.name}`;
}

/** roleBindingOwnerLabel returns the owner with its kind, for a badge. */
export function roleBindingOwnerLabel(owner: RoleBindingOwner): string {
  const kind = owner.kind === "TenantAgent" ? "Tenant agent" : "Brood schedule";
  return `${kind}: ${roleBindingOwnerRef(owner)}`;
}

/** roleBindingOwnerNoun returns the object as the note names it. */
export function roleBindingOwnerNoun(owner: RoleBindingOwner): string {
  return owner.kind === "TenantAgent" ? "the Tenant agent" : "the brood schedule";
}

/** roleBindingOwnerAdvice tells the reader which object to change. */
export function roleBindingOwnerAdvice(owner: RoleBindingOwner): string {
  return owner.kind === "TenantAgent" ? "Change the TenantAgent object instead." : "Change the BroodSchedule instead.";
}

/** roleBindingOwnerSummary returns one line that names the owner. A badge shows it as its title. */
export function roleBindingOwnerSummary(owner: RoleBindingOwner): string {
  return `The operator writes this binding for ${roleBindingOwnerNoun(owner)} ${roleBindingOwnerRef(owner)}. ${roleBindingOwnerAdvice(owner)}`;
}

/**
 * roleBindingOwnerRoute returns the page of the owner, or null when no page
 * shows it. A TenantAgent links to its agent page only when its binding names
 * exactly one Tenant.
 */
export function roleBindingOwnerRoute(owner: RoleBindingOwner, spec?: VarroaRoleBindingSpec): string | null {
  if (owner.kind === "BroodSchedule") {
    return `/brood-schedules/${encodeURIComponent(owner.namespace)}/${encodeURIComponent(owner.name)}`;
  }
  const tenants = spec?.scope?.tenants ?? [];
  if (tenants.length !== 1) return null;
  return `/pipelines/${encodeURIComponent(tenants[0].name)}/agent`;
}
