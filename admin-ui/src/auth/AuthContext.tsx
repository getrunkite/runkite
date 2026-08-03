import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import {
  clearLegacyCredentials,
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
    clearLegacyCredentials();
    fetchSessionStatus().then(
      (s) => {
        setAuthRequired(s.auth_required);
        if (s.csrf_token) setCSRFToken(s.csrf_token);
        setStatus(s.authenticated ? "authenticated" : "unauthenticated");
      },
      () => {
        setAuthRequired(true);
        setStatus("unauthenticated");
      },
    );
  }, []);

  async function login(token: string) {
    const s = await createSession(token);
    if (s.csrf_token) setCSRFToken(s.csrf_token);
    setAuthRequired(true);
    setStatus("authenticated");
  }

  function logout() {
    void destroySession().finally(() => {
      setCSRFToken(null);
      if (!authRequired) {
        setStatus("authenticated");
        return;
      }
      setStatus("unauthenticated");
    });
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
