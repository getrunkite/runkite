// Thin fetch wrapper for the /admin-api/* surface (internal/api/admin.go).
// A relative base path works both in dev (Vite proxies /admin-api to the
// control plane unchanged, see vite.config.ts) and in production (the
// built bundle is served from the same origin as the API -- see
// internal/adminui's embed.FS wiring).
//
// Auth: httpOnly session cookie set by POST /admin-api/session. The API
// key/JWT is sent once at login and never stored in JavaScript. Mutating
// calls send X-CSRF-Token (synchronizer token from the login/status
// response). Machine clients can still use Authorization: Bearer.

const BASE = "/admin-api";

const LEGACY_CREDENTIAL_KEY = "runkite_admin_credential";

/** CSRF synchronizer token for cookie-authenticated mutations. Held in
 * module memory (and optionally rehydrated from GET /admin-api/session
 * after refresh) -- not the API key. */
let csrfToken: string | null = null;

export function getCSRFToken(): string | null {
  return csrfToken;
}

export function setCSRFToken(token: string | null): void {
  csrfToken = token;
}

/** Drop any pre-httpOnly credential left in web storage. */
export function clearLegacyCredentials(): void {
  try {
    sessionStorage.removeItem(LEGACY_CREDENTIAL_KEY);
    localStorage.removeItem(LEGACY_CREDENTIAL_KEY);
  } catch {
    // private mode / disabled storage -- nothing to clear
  }
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export interface ApiResponse<T> {
  data: T;
  /** Opaque Admin list resume token from X-Next-Cursor, when present. */
  nextCursor: string | null;
}

async function request<T>(path: string, init?: RequestInit): Promise<ApiResponse<T>> {
  const headers = new Headers(init?.headers);
  const method = (init?.method ?? "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD" && csrfToken) {
    headers.set("X-CSRF-Token", csrfToken);
  }

  const resp = await fetch(`${BASE}${path}`, {
    ...init,
    headers,
    credentials: "include",
  });
  if (!resp.ok) {
    let message = `${resp.status} ${resp.statusText}`;
    try {
      const body = await resp.json();
      if (body?.message) message = body.message;
    } catch {
      // response body wasn't JSON -- keep the status line as the message
    }
    throw new ApiError(resp.status, message);
  }
  const nextCursor = resp.headers.get("X-Next-Cursor");
  if (resp.status === 204) return { data: undefined as T, nextCursor: null };
  return { data: (await resp.json()) as T, nextCursor };
}

async function requestData<T>(path: string, init?: RequestInit): Promise<T> {
  const { data } = await request<T>(path, init);
  return data;
}

export const api = {
  get: <T>(path: string) => requestData<T>(path),
  /** Like get, but also returns X-Next-Cursor for Admin keyset paging. */
  getWithMeta: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    requestData<T>(path, {
      method: "POST",
      headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    }),
  del: <T>(path: string) => requestData<T>(path, { method: "DELETE" }),
};

export interface SessionStatus {
  authenticated: boolean;
  auth_required: boolean;
  csrf_token?: string;
  identity?: string;
}

/** Probe / create session state without storing the API key in JS. */
export async function fetchSessionStatus(): Promise<SessionStatus> {
  const resp = await fetch(`${BASE}/session`, { credentials: "include" });
  if (!resp.ok) {
    throw new ApiError(resp.status, `Unexpected response: ${resp.status}`);
  }
  return resp.json() as Promise<SessionStatus>;
}

/** Exchange a pasted credential for an httpOnly session cookie + CSRF. */
export async function createSession(credential: string): Promise<SessionStatus> {
  const resp = await fetch(`${BASE}/session`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ credential }),
  });
  if (!resp.ok) {
    let message = `${resp.status} ${resp.statusText}`;
    try {
      const body = await resp.json();
      if (body?.message) message = body.message;
    } catch {
      // keep status line
    }
    if (resp.status === 401 || resp.status === 403) {
      throw new ApiError(resp.status, message || "Invalid credential or missing 'admin' permission.");
    }
    throw new ApiError(resp.status, message);
  }
  return resp.json() as Promise<SessionStatus>;
}

export async function destroySession(): Promise<void> {
  const headers = new Headers();
  if (csrfToken) headers.set("X-CSRF-Token", csrfToken);
  await fetch(`${BASE}/session`, {
    method: "DELETE",
    credentials: "include",
    headers,
  });
  csrfToken = null;
}
