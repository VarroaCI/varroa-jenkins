import { BFF_BASE } from "../api/client";

const STATUS_COPY: Record<number, string> = {
  400: "The request could not be completed.",
  401: "Your session has expired.",
  403: "You do not have access to this page.",
  404: "We could not find that page.",
  409: "The request conflicts with the current state.",
  429: "Too many requests. Please try again shortly.",
};
const CODE_COPY: Record<string, string> = { PREFLIGHT_FAILED: "Preflight checks failed." };

export class ApiError extends Error {
  constructor(public readonly status: number, message: string, public readonly body?: unknown) {
    super(message);
    this.name = "ApiError";
  }
}

function getToken(): string | null {
  try { return localStorage.getItem("varroa_id_token"); } catch { return null; }
}

async function errorFromResponse(res: Response): Promise<ApiError> {
  let body: unknown;
  if ((res.headers.get("content-type") || "").includes("application/json")) {
    try { body = await res.json(); } catch { body = undefined; }
  }
  const code = body && typeof body === "object" && "code" in body && typeof body.code === "string" ? body.code : "";
  const message = CODE_COPY[code] || STATUS_COPY[res.status] || "Varroa could not complete the request.";
  return new ApiError(res.status, message, body);
}

async function request(path: string, options?: RequestInit): Promise<Response> {
  const token = getToken();
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (options?.headers) Object.assign(headers, options.headers);
  const { headers: _, ...rest } = options ?? {};
  let res: Response;
  try { res = await fetch(`${BFF_BASE}${path}`, { headers, ...rest }); }
  catch { throw new ApiError(0, "Varroa could not load this page."); }
  if (!res.ok) {
    const error = await errorFromResponse(res);
    if (error.status === 401) window.dispatchEvent(new Event("varroa:unauthorized"));
    throw error;
  }
  return res;
}

async function bffFetch<T>(path: string, options?: RequestInit): Promise<T> {
  const res = await request(path, options);
  if (res.status === 204 || res.status === 202) return undefined as T;
  return res.json();
}

async function bffFetchText(path: string, options?: RequestInit): Promise<string> {
  return (await request(path, options)).text();
}

/**
 * POSTs a FormData body. The Content-Type header is deliberately OMITTED so the
 * browser sets `multipart/form-data; boundary=...` itself — a hardcoded
 * Content-Type would leave the boundary out and the server could not parse the
 * body. The Authorization header and the 401 dispatch are preserved.
 */
async function bffUpload<T>(path: string, body: FormData): Promise<T> {
  const token = getToken();
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = `Bearer ${token}`;

  let res: Response;
  try { res = await fetch(`${BFF_BASE}${path}`, { method: "POST", headers, body }); }
  catch { throw new ApiError(0, "Varroa could not reach the update center."); }

  if (!res.ok) {
    // The rejection body carries the per-dependency diff, so it is attached to
    // the ApiError rather than collapsed into a status message.
    const error = await errorFromResponse(res);
    if (error.status === 401) window.dispatchEvent(new Event("varroa:unauthorized"));
    throw error;
  }
  return res.json();
}

async function logout(): Promise<{ redirect?: string }> {
  return bffFetch("/logout", { method: "POST" });
}

export { bffFetch, bffFetchText, bffUpload, getToken, logout };
