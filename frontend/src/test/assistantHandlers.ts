// MSW handlers for the assistant and run note routes. Each call records its
// method, path, and body in the log, so a test can assert a request was or
// was not sent.
import { http, HttpResponse } from "msw";
import type { AssistantSession } from "../types/assistant";
import {
  createAssistantApproval,
  createAssistantSession,
  createAssistantSettings,
  createMyAssistant,
  createRunNote,
} from "./assistantFactories";

const BASE = "/api/v1";

export interface AssistantHandlerOptions {
  me?: ReturnType<typeof createMyAssistant> | "forbidden";
  settings?: ReturnType<typeof createAssistantSettings>;
  session?: AssistantSession;
  notes?: ReturnType<typeof createRunNote>[];
}

export interface AssistantRequestLog {
  method: string;
  path: string;
  body: unknown;
}

/** assistantHandlers returns the default assistant route set and its request log. */
export function assistantHandlers(opts: AssistantHandlerOptions = {}) {
  const log: AssistantRequestLog[] = [];
  const record = async (req: Request) => {
    const text = req.method === "GET" || req.method === "DELETE" ? "" : await req.clone().text();
    let body: unknown = undefined;
    try { body = text ? JSON.parse(text) : undefined; } catch { body = text; }
    log.push({ method: req.method, path: new URL(req.url).pathname, body });
  };
  const session = opts.session ?? createAssistantSession();

  const handlers = [
    http.get(`${BASE}/me/assistant`, async ({ request }) => {
      await record(request);
      if (opts.me === "forbidden") return new HttpResponse(null, { status: 403 });
      return HttpResponse.json(opts.me ?? createMyAssistant());
    }),
    http.put(`${BASE}/me/assistant`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(opts.me && opts.me !== "forbidden" ? opts.me : createMyAssistant());
    }),
    http.put(`${BASE}/me/assistant/key`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(createMyAssistant({ sessionKey: { provider: "smoke" } }));
    }),
    http.delete(`${BASE}/me/assistant/key`, async ({ request }) => {
      await record(request);
      return new HttpResponse(null, { status: 204 });
    }),
    http.get(`${BASE}/assistant/settings`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(opts.settings ?? createAssistantSettings());
    }),
    http.put(`${BASE}/assistant/settings`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(opts.settings ?? createAssistantSettings());
    }),
    http.get(`${BASE}/assistant/providers`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({
        items: [{ name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://127.0.0.1:8082/v1" }],
      });
    }),
    http.post(`${BASE}/assistant/sessions`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(session, { status: 201 });
    }),
    http.get(`${BASE}/assistant/sessions`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ items: [session], nextCursor: null });
    }),
    http.get(`${BASE}/assistant/sessions/:id`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ session, approvals: [createAssistantApproval({ state: "pending" })] });
    }),
    http.post(`${BASE}/assistant/sessions/:id/messages`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ state: "running" }, { status: 202 });
    }),
    http.post(`${BASE}/assistant/sessions/:id/cancel`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ state: "cancelled" }, { status: 202 });
    }),
    http.get(`${BASE}/assistant/approvals`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ items: [createAssistantApproval()], nextCursor: null });
    }),
    http.post(`${BASE}/assistant/approvals/:id/approve`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ approval: createAssistantApproval({ state: "approved" }), result: { content: "ok", isError: false } }, { status: 200 });
    }),
    http.post(`${BASE}/assistant/approvals/:id/deny`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ approval: createAssistantApproval({ state: "denied" }), result: { content: "denied", isError: true } }, { status: 200 });
    }),
    http.get(`${BASE}/runs/:run/notes`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ items: opts.notes ?? [] });
    }),
    http.post(`${BASE}/runs/:run/notes`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(createRunNote({ body: "posted" }), { status: 201 });
    }),
    http.get(`${BASE}/pipelines/:tenant/jobs/:job/notes`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ items: opts.notes ?? [] });
    }),
  ];
  return { handlers, log };
}
