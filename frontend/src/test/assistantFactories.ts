// Test factories for the assistant and run note shapes.
import type {
  AssistantApproval,
  AssistantSession,
  AssistantConfigObject,
  AssistantSettings,
  MyAssistant,
  RunNote,
} from "../types/assistant";

export function createAssistantSession(overrides: Partial<AssistantSession> = {}): AssistantSession {
  return {
    id: "00000000-0000-7000-8000-000000000001",
    kind: "assistant",
    principal: "alice",
    tenant: "",
    cluster: "core",
    trigger: "manual",
    context: { route: "dashboard" },
    firstMessage: "",
    provider: "smoke",
    model: "varroa-smoke",
    keySource: "chart",
    dataSharing: "logs",
    state: "running",
    skipReason: null,
    promptTokens: 0,
    completionTokens: 0,
    steps: 0,
    startedAt: "2026-10-18T10:00:00Z",
    endedAt: null,
    ...overrides,
  };
}

export function createAssistantApproval(overrides: Partial<AssistantApproval> = {}): AssistantApproval {
  return {
    id: "00000000-0000-7000-8000-0000000000a1",
    sessionId: "00000000-0000-7000-8000-000000000001",
    callId: "call-1",
    tool: "restart_run",
    arguments: { run: "r1" },
    targetKind: "run",
    targetRef: "r1",
    state: "pending",
    requestedAt: "2026-10-18T10:00:00Z",
    expiresAt: "2099-01-01T00:00:00Z",
    resolvedBy: null,
    resolvedAt: null,
    reason: null,
    result: null,
    resultWithheld: false,
    ...overrides,
  };
}

export function createMyAssistant(overrides: Partial<MyAssistant> = {}): MyAssistant {
  return {
    available: true,
    enabled: true,
    provider: "smoke",
    model: "varroa-smoke",
    dataSharing: "logs",
    sessionKey: null,
    effectiveLevels: { core: "logs" },
    dayTokens: 0,
    dayBudget: 100000,
    ...overrides,
  };
}

export function createAssistantSettings(overrides: Partial<AssistantSettings> = {}): AssistantSettings {
  return {
    paused: false,
    pausedBy: null,
    pausedAt: null,
    tenantDataSharing: {},
    updatedAt: "2026-10-18T10:00:00Z",
    updatedBy: "admin",
    ...overrides,
  };
}

/** createAssistantConfig is the AssistantConfig object with one listed provider. */
export function createAssistantConfig(overrides: Partial<AssistantConfigObject> = {}): AssistantConfigObject {
  return {
    apiVersion: "varroa.dev/v1alpha1",
    kind: "AssistantConfig",
    metadata: { name: "varroa-assistant", resourceVersion: "7" },
    spec: {
      providers: [{
        name: "smoke", kind: "openai-compatible", baseURL: "http://192.168.1.99:8082/v1",
        models: ["varroa-smoke"], trust: "internal", auth: "none", cidrs: ["192.168.1.99/32"], port: 8082,
      }],
      defaultProvider: "smoke",
      defaultModel: "varroa-smoke",
      externalDataSharing: "logs",
      budgets: { session: 50000, userDay: 200000, tenantDay: 500000, steps: 16 },
      approvalExpiry: "1h",
    },
    ...overrides,
  };
}

export function createRunNote(overrides: Partial<RunNote> = {}): RunNote {
  return {
    id: "00000000-0000-7000-8000-0000000000e1",
    runId: "00000000-0000-7000-8000-0000000000b1",
    cluster: "core",
    tenant: "payments",
    jobPath: "payments/api",
    principal: "alice",
    sessionId: null,
    body: "a human note",
    createdAt: "2026-10-18T10:00:00Z",
    ...overrides,
  };
}

/** createAssistantFrames is the stream of a short completed turn. */
export function createAssistantFrames(): { event: string; id: string; data: Record<string, unknown> }[] {
  const at = "2026-10-18T10:00:00Z";
  return [
    { event: "user", id: "1", data: { text: "why did it fail", created_at: at } },
    { event: "assistant", id: "2", data: { turn: 1, text: "It timed out.", final: true, created_at: at } },
    { event: "done", id: "3", data: { state: "completed", stop: "", usage_estimated: false, created_at: at } },
  ];
}
