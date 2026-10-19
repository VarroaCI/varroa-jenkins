import { describe, it, expect } from "vitest";
import { isAgentRow, passesSource, SOURCE_LABELS } from "../activityTimeline.util";
import { orderApprovals } from "./TargetApprovalsCard";
import { createAssistantApproval } from "../../test/assistantFactories";
import type { ActivityEvent } from "../../types";

const base = { id: "e", source: "user", actor: "alice" } as unknown as ActivityEvent;

describe("isAgentRow", () => {
  it("accepts an assistant source, a team agent source, and an agent actor", () => {
    expect(isAgentRow({ ...base, source: "assistant" } as ActivityEvent)).toBe(true);
    expect(isAgentRow({ ...base, source: "team_agent" } as ActivityEvent)).toBe(true);
    expect(isAgentRow({ ...base, actor: "agent:team-a" } as ActivityEvent)).toBe(true);
  });

  it("rejects a user row", () => {
    expect(isAgentRow(base)).toBe(false);
  });

  it("filters the Agents chip and labels the new sources", () => {
    expect(passesSource({ ...base, source: "assistant" } as ActivityEvent, "agents")).toBe(true);
    expect(passesSource(base, "agents")).toBe(false);
    expect(SOURCE_LABELS.assistant).toBe("Assistant");
    expect(SOURCE_LABELS.team_agent).toBe("Team agent");
  });
});

describe("orderApprovals", () => {
  it("puts pending first and then the newest resolved ones", () => {
    const items = [
      createAssistantApproval({ id: "old", state: "approved", resolvedAt: "2026-10-01T00:00:00Z" }),
      createAssistantApproval({ id: "p", state: "pending" }),
      createAssistantApproval({ id: "new", state: "denied", resolvedAt: "2026-10-10T00:00:00Z" }),
    ];
    expect(orderApprovals(items).map((a) => a.id)).toEqual(["p", "new", "old"]);
  });

  it("keeps at most ten resolved approvals", () => {
    const items = Array.from({ length: 12 }, (_, i) =>
      createAssistantApproval({ id: `r${i}`, state: "approved", resolvedAt: `2026-10-${String(i + 1).padStart(2, "0")}T00:00:00Z` }));
    expect(orderApprovals(items)).toHaveLength(10);
  });
});
