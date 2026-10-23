// Types of the Tenant agent routes. A Tenant agent is the TenantAgent object that
// the acceptance rule selects for one pipelines Tenant.
import type { AssistantApproval, AssistantSession } from "./assistant";

export interface TenantAgentCondition {
  type: string;
  status: "True" | "False" | "Unknown";
  reason: string;
  message?: string;
  lastTransitionTime: string;
  observedGeneration?: number;
}

export interface TenantAgentSpec {
  tenantRef: { name: string };
  enabled?: boolean;
  provider?: string;
  model?: string;
  instructions?: { source: string; path?: string };
  triggers?: { type: string; debounce?: string }[];
  unattendedWrites?: string[];
  outputs?: {
    notes?: boolean;
    issues?: { repo: string; forge: string; labels?: string[] };
    webhook?: boolean;
  };
  budgets?: { session?: number; day?: number };
  dataSharing?: "none" | "metadata" | "logs";
}

export interface TenantAgent {
  namespace: string;
  name: string;
  tenant: string;
  cluster: string;
  spec: TenantAgentSpec;
  status: {
    conditions?: TenantAgentCondition[];
    instructionsSHA?: string;
    lastSession?: { id: string; trigger: string; state: string; at: string };
    spendToday?: number;
    observedGeneration?: number;
  };
  accepted: boolean;
  pendingApprovals: number;
  spendToday: number;
  dayLimit: number;
}

export interface TenantAgentSession extends AssistantSession {
  pendingApprovals: AssistantApproval[];
}

export interface TenantAgentSessionList {
  items: TenantAgentSession[];
  nextCursor: string;
}

export interface TenantAgentCapability {
  name: string;
  description: string;
  kind: string;
}

export interface TenantAgentCapabilities {
  reads: TenantAgentCapability[];
  writesByApproval: TenantAgentCapability[];
  unattended: TenantAgentCapability[];
  never: TenantAgentCapability[];
}

export interface TenantAgentItem {
  namespace: string;
  name: string;
  tenant: string;
  cluster: string;
  enabled: boolean;
  paused: { reason: string; message: string } | null;
  sessionsToday: number;
  spendToday: number;
  failedToday: number;
  conditions: TenantAgentCondition[];
  unattendedRequested: string[];
  unattendedGranted: string[];
}

export interface TenantAgentItemList {
  items: TenantAgentItem[];
}

export interface TenantAgentUnattendedResult {
  namespace: string;
  name: string;
  enabled: boolean;
  unattendedGranted: string[];
}
