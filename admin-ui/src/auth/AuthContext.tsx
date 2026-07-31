import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { clearStoredCredential, getStoredCredential, setStoredCredential, verifyCredential } from "../api/client";

interface AuthState {
  // "checking": haven't determined yet whether a login is even needed
  // (see the no-auth-configured probe below). "authenticated": either a
  // verified credential is stored, or the control plane has no auth
  // provider configured at all (local/dev mode) -- the dashboard doesn't
  // force a login screen on top of a deployment that has no auth to log
  // in to. "unauthenticated": a login is required and none is stored yet.
  status: "checking" | "authenticated" | "unauthenticated";
  // False when the empty-token probe succeeded -- the control plane is
  // open (no client-facing auth). Sign-out must not trap the operator
  // on the Login form in that mode.
  authRequired: boolean;
  login: (token: string) => Promise<void>;
  logout: () => void;
}

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthState["status"]>("checking");
  const [authRequired, setAuthRequired] = useState(true);

  useEffect(() => {
    const stored = getStoredCredential();
    if (stored) {
      // Trust a previously-verified credential optimistically; if it's
      // since been revoked, the first real API call will 401/403 and the
      // relevant page shows that error rather than silently redirecting
      // (a stale redirect loop is worse UX than a visible error here).
      setAuthRequired(true);
      setStatus("authenticated");
      return;
    }
    // No credential stored -- probe whether auth is even configured. A
    // deployment with no auth provider (local/dev mode, see README's Auth
    // section) answers every request the same regardless of credentials,
    // so this doubles as "is a login required at all".
    verifyCredential("").then(
      () => {
        setAuthRequired(false);
        setStatus("authenticated");
      },
      () => {
        setAuthRequired(true);
        setStatus("unauthenticated");
      },
    );
  }, []);

  async function login(token: string) {
    await verifyCredential(token);
    setStoredCredential(token);
    setAuthRequired(true);
    setStatus("authenticated");
  }

  function logout() {
    clearStoredCredential();
    if (!authRequired) {
      // Open deployment: clearing storage must not land on Login, which
      // requires a non-empty token and has nowhere useful to go.
      setStatus("authenticated");
      return;
    }
    setStatus("unauthenticated");
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
