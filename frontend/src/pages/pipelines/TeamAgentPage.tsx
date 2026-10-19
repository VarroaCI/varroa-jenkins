// TeamAgentPage shows the Team agent of one pipelines Team: its sessions, its
// approvals, its capabilities, and its settings. A session id in the query
// opens the full-page transcript. The page needs the tenant read check; the
// server answers 404 when the Team has no accepted Team agent.
import { useState } from "react";
import { useParams, useSearchParams, Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { getAssistantSession, getTeamAgent, listTeamAgentSessions } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { Tabs, tabPanelProps } from "../../components/Tabs";
import { Card } from "../../components/Card";
import { TranscriptStream } from "../../components/assistant/TranscriptStream";
import { TranscriptView } from "../../components/assistant/TranscriptView";
import { emptyModel, type TranscriptModel } from "../../components/assistant/transcript";
import { TeamAgentApprovals } from "./TeamAgentApprovals";
import { TeamAgentCapabilities } from "./TeamAgentCapabilities";
import { TeamAgentSettings } from "./TeamAgentSettings";
import { age } from "../../components/activityTimeline.util";
import styles from "./TeamAgentPage.module.css";

const TABS = [
  { id: "sessions", label: "Sessions" },
  { id: "approvals", label: "Approvals" },
  { id: "capabilities", label: "Capabilities" },
  { id: "settings", label: "Settings" },
];

/** TeamAgentPage renders the Team agent page of one tenant. */
export default function TeamAgentPage() {
  const { team = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const sessionId = params.get("session");
  const tabParam = params.get("tab") ?? "sessions";
  const activeTab = TABS.some((t) => t.id === tabParam) ? tabParam : "sessions";
  useDocumentTitle(`${team} · Team agent`);

  const agent = useQuery({
    queryKey: ["team-agent", team],
    queryFn: () => getTeamAgent(team),
    retry: (count, err) => !(err instanceof ApiError && err.status === 404) && count < 2,
  });

  if (agent.error instanceof ApiError && agent.error.status === 404) {
    return (
      <div className={styles.page}>
        <h1 className={styles.title}>Team agent</h1>
        <p role="status">This Team has no Team agent.</p>
      </div>
    );
  }
  if (agent.isPending) return <div className={styles.page}>Loading the Team agent…</div>;
  if (agent.error) return <div className={styles.page} role="alert">The Team agent could not be read.</div>;
  const ta = agent.data;

  if (sessionId) {
    return (
      <div className={styles.page}>
        <nav className={styles.crumbs}>
          <Link to={`/pipelines/${encodeURIComponent(team)}/agent`}>Team agent</Link>
        </nav>
        <h1 className={styles.title}>Session {sessionId.slice(0, 8)}</h1>
        <FullTranscript sessionId={sessionId} />
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <h1 className={styles.title}>Team agent · {team}</h1>
      <Tabs
        tabs={TABS}
        activeTab={activeTab}
        idBase="team-agent"
        ariaLabel="Team agent sections"
        onSelect={(id) => setParams((p) => {
          const next = new URLSearchParams(p);
          next.set("tab", id);
          return next;
        })}
      />
      <div {...tabPanelProps("team-agent", activeTab)}>
        {activeTab === "sessions" && <SessionsTab team={team} onOpen={(id) => setParams({ session: id })} />}
        {activeTab === "approvals" && <TeamAgentApprovals team={team} />}
        {activeTab === "capabilities" && <TeamAgentCapabilities team={team} />}
        {activeTab === "settings" && <TeamAgentSettings agent={ta} />}
      </div>
    </div>
  );
}

/** SessionsTab lists the Team agent sessions, newest first. */
function SessionsTab({ team, onOpen }: { team: string; onOpen: (id: string) => void }) {
  const [state, setState] = useState("");
  const list = useQuery({
    queryKey: ["team-agent-sessions", team, state],
    queryFn: () => listTeamAgentSessions(team, { state }),
  });
  return (
    <Card
      title="Sessions"
      headerRight={
        <select aria-label="Filter by state" value={state} onChange={(e) => setState(e.target.value)}>
          <option value="">All states</option>
          <option value="running">Running</option>
          <option value="waiting_approval">Waiting for approval</option>
          <option value="completed">Completed</option>
          <option value="failed">Failed</option>
          <option value="skipped">Skipped</option>
        </select>
      }
    >
      {list.isPending && <p>Loading sessions…</p>}
      {list.data && list.data.items.length === 0 && <p>No sessions yet.</p>}
      {list.data && list.data.items.length > 0 && (
        <table className={styles.table}>
          <thead>
            <tr>
              <th scope="col">Started</th>
              <th scope="col">Trigger</th>
              <th scope="col">Item</th>
              <th scope="col">State</th>
              <th scope="col">Tokens</th>
              <th scope="col">Steps</th>
            </tr>
          </thead>
          <tbody>
            {list.data.items.map((s) => {
              const job = typeof s.context?.job === "string" ? s.context.job : "";
              return (
                <tr key={s.id}>
                  <td>
                    <button type="button" className={styles.link} onClick={() => onOpen(s.id)}>
                      {age(s.startedAt)}
                    </button>
                  </td>
                  <td>{s.trigger}</td>
                  <td>{job || s.team}</td>
                  <td>{s.state}</td>
                  <td>{s.promptTokens + s.completionTokens}</td>
                  <td>{s.steps}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </Card>
  );
}

/** FullTranscript streams one session into the full-page transcript view. */
function FullTranscript({ sessionId }: { sessionId: string }) {
  const [model, setModel] = useState<TranscriptModel>(emptyModel());
  const detail = useQuery({
    queryKey: ["assistant-session", sessionId],
    queryFn: () => getAssistantSession(sessionId),
    retry: false,
  });
  if (detail.error instanceof ApiError && detail.error.status === 404) {
    return <p role="status">This session is not available.</p>;
  }
  return (
    <div className={styles.transcript}>
      <TranscriptStream sessionId={sessionId} resumeFrom={0} onModel={setModel} onDone={() => undefined} />
      <TranscriptView model={model} />
      {detail.data && <p className={styles.meta}>State: {detail.data.session.state}</p>}
    </div>
  );
}
