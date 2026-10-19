// Types of the Team agent routes. A Team agent is the TeamAgent object that
// the acceptance rule selects for one pipelines Team.
import type { AssistantApproval, AssistantSession } from "./assistant";

export interface TeamAgentCondition {
  type: string;
  status: "True" | "False" | "Unknown";
  reason: string;
  message?: string;
  lastTransitionTime: string;
  observedGeneration?: number;
}

export interface TeamAgentSpec {
  teamRef: { name: string };
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

export interface TeamAgent {
  namespace: string;
  name: string;
  team: string;
  cluster: string;
  spec: TeamAgentSpec;
  status: {
    conditions?: TeamAgentCondition[];
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

export interface TeamAgentSession extends AssistantSession {
  pendingApprovals: AssistantApproval[];
}

export interface TeamAgentSessionList {
  items: TeamAgentSession[];
  nextCursor: string;
}

export interface TeamAgentCapability {
  name: string;
  description: string;
  kind: string;
}

export interface TeamAgentCapabilities {
  reads: TeamAgentCapability[];
  writesByApproval: TeamAgentCapability[];
  unattended: TeamAgentCapability[];
  never: TeamAgentCapability[];
}

export interface TeamAgentItem {
  namespace: string;
  name: string;
  team: string;
  cluster: string;
  enabled: boolean;
  paused: { reason: string; message: string } | null;
  sessionsToday: number;
  spendToday: number;
  failedToday: number;
  conditions: TeamAgentCondition[];
  unattendedRequested: string[];
  unattendedGranted: string[];
}

export interface TeamAgentItemList {
  items: TeamAgentItem[];
}

export interface TeamAgentUnattendedResult {
  namespace: string;
  name: string;
  enabled: boolean;
  unattendedGranted: string[];
}
