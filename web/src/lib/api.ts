// API client: JSON in and out, CSRF token and an Idempotency-Key on every change.
export class ApiError extends Error {
  status: number;
  code: string;
  remediation?: string;
  constructor(status: number, code: string, message: string, remediation?: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.remediation = remediation;
  }
}

let csrf = "";
export function setCsrf(t: string) { csrf = t; }
let onUnauthenticated: () => void = () => {};
export function setUnauthenticatedHandler(f: () => void) { onUnauthenticated = f; }

export function newKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return Math.random().toString(36).slice(2) + Date.now().toString(36);
}

async function request<T>(method: string, path: string, body?: unknown, key?: string): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") {
    headers["X-CSRF-Token"] = csrf;
    headers["Idempotency-Key"] = key || newKey();
  }
  let res: Response;
  try {
    res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin" });
  } catch {
    throw new ApiError(0, "network", "Upwell is not reachable. Check that the service is running and your network connection.");
  }
  const text = await res.text();
  let data: any = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = text; }
  }
  if (!res.ok) {
    if (res.status === 401 && path !== "/api/v1/auth/login") onUnauthenticated();
    const d = data && typeof data === "object" ? data : {};
    throw new ApiError(res.status, d.code || "error", d.message || `Request failed (${res.status})`, d.remediation);
  }
  return data as T;
}

export const api = {
  get: <T,>(path: string) => request<T>("GET", path),
  post: <T,>(path: string, body?: unknown, key?: string) => request<T>("POST", path, body ?? {}, key),
  put: <T,>(path: string, body?: unknown) => request<T>("PUT", path, body ?? {}),
  patch: <T,>(path: string, body?: unknown) => request<T>("PATCH", path, body ?? {}),
  del: <T,>(path: string) => request<T>("DELETE", path),
};
