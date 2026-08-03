// Thin fetch wrapper for the /admin-api/* surface (internal/api/admin.go).
// A relative base path works both in dev (Vite proxies /admin-api to the
// control plane unchanged, see vite.config.ts) and in production (the
// built bundle is served from the same origin as the API -- see
// internal/adminui's embed.FS wiring).

const BASE = "/admin-api";

const CREDENTIAL_STORAGE_KEY = "runkite_admin_credential";

/** The credential is whatever the client-facing auth provider expects in
 * an Authorization: Bearer header -- a static API key, or a JWT. The
 * dashboard doesn't know or care which; it just needs "admin" permission
 * on whichever one the operator pastes in (see Login.tsx).
 *
 * sessionStorage (not localStorage): survives refresh within the tab,
 * not new tabs or after the browser session ends -- shrinks the window
 * where a stolen XSS can read a long-lived token. Still XSS-readable
 * in-tab; an httpOnly cookie session would close that (not implemented). */
export function getStoredCredential(): string | null {
  const fromSession = sessionStorage.getItem(CREDENTIAL_STORAGE_KEY);
  if (fromSession) return fromSession;
  // One-time migrate older localStorage credentials into sessionStorage.
  const legacy = localStorage.getItem(CREDENTIAL_STORAGE_KEY);
  if (legacy) {
    sessionStorage.setItem(CREDENTIAL_STORAGE_KEY, legacy);
    localStorage.removeItem(CREDENTIAL_STORAGE_KEY);
    return legacy;
  }
  return null;
}

export function setStoredCredential(token: string): void {
  sessionStorage.setItem(CREDENTIAL_STORAGE_KEY, token);
  localStorage.removeItem(CREDENTIAL_STORAGE_KEY);
}

export function clearStoredCredential(): void {
  sessionStorage.removeItem(CREDENTIAL_STORAGE_KEY);
  localStorage.removeItem(CREDENTIAL_STORAGE_KEY);
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const credential = getStoredCredential();
  const headers = new Headers(init?.headers);
  if (credential) headers.set("Authorization", `Bearer ${credential}`);

  const resp = await fetch(`${BASE}${path}`, { ...init, headers });
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
  if (resp.status === 204) return undefined as T;
  return resp.json() as Promise<T>;
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, {
      method: "POST",
      headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    }),
  del: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};

/** Verifies a credential actually has admin access before storing it --
 * Login.tsx calls this so a bad key fails fast with a clear message
 * instead of silently landing on a dashboard full of 403s. */
export async function verifyCredential(token: string): Promise<void> {
  const resp = await fetch(`${BASE}/overview`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!resp.ok) {
    if (resp.status === 401 || resp.status === 403) {
      throw new ApiError(resp.status, "Invalid credential or missing 'admin' permission.");
    }
    throw new ApiError(resp.status, `Unexpected response: ${resp.status}`);
  }
}
