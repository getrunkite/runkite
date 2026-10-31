import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { toast } from "sonner";
import {
  clearLegacyCredentials,
  ApiError,
  createSession,
  destroySession,
  fetchSessionStatus,
  setCSRFToken,
} from "../api/client";

interface AuthState {
  // "checking": haven't determined yet whether a login is even needed
  // (see the session status probe below). "authenticated": either a
  // live httpOnly session cookie exists, or the control plane has no
  // auth provider configured at all (local/dev mode). "unauthenticated":
  // a login is required and no session is active.
  status: "checking" | "authenticated" | "unauthenticated";
  // False when the empty-session probe reports auth_required=false --
  // the control plane is open. Sign-out must not trap the operator on
  // the Login form in that mode.
  authRequired: boolean;
  login: (token: string) => Promise<void>;
  logout: () => void;
}

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthState["status"]>("checking");
  const [authRequired, setAuthRequired] = useState(true);

  useEffect(() => {
    let active = true;
    clearLegacyCredentials();
    fetchSessionStatus().then(
      (s) => {
        if (!active) return;
        setAuthRequired(s.auth_required);
        setCSRFToken(s.csrf_token ?? null);
        setStatus(s.authenticated ? "authenticated" : "unauthenticated");
      },
      () => {
        if (!active) return;
        setCSRFToken(null);
        setAuthRequired(true);
        setStatus("unauthenticated");
      },
    );
    return () => { active = false; };
  }, []);

  async function login(token: string) {
    const s = await createSession(token);
    setCSRFToken(s.csrf_token ?? null);
    setAuthRequired(true);
    setStatus("authenticated");
  }

  function logout() {
    void destroySession().then(
      () => setStatus(authRequired ? "unauthenticated" : "authenticated"),
      (err) => toast.error("Sign out was not confirmed", {
        description: err instanceof ApiError ? err.message : "Your session may still be active. Try again.",
      }),
    );
  }

  return (
    <AuthContext.Provider value={{ status, authRequired, login, logout }}>{children}</AuthContext.Provider>
  );
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within an AuthProvider");
  return ctx;
}
