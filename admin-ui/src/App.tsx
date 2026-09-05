import { Navigate, Route, Routes } from "react-router";
import { Loader2 } from "lucide-react";
import { AuthProvider, useAuth } from "./auth/AuthContext";
import { Layout } from "./components/Layout";
import { Login } from "./pages/Login";
import { Overview } from "./pages/Overview";
import { Agents } from "./pages/Agents";
import { AgentDetail } from "./pages/AgentDetail";
import { Registry } from "./pages/Registry";
import { RegistryEntry } from "./pages/RegistryEntry";
import { Threads } from "./pages/Threads";
import { ThreadDetail } from "./pages/ThreadDetail";
import { Runs } from "./pages/Runs";
import { RunDetail } from "./pages/RunDetail";
import { Connectors } from "./pages/Connectors";
import { Cron } from "./pages/Cron";
import { Webhooks } from "./pages/Webhooks";
import { PolicyGrants } from "./pages/PolicyGrants";
import { PendingActions } from "./pages/PendingActions";
import { KillSwitches } from "./pages/KillSwitches";
import { BreakGlass } from "./pages/BreakGlass";
import { MandatoryHITL } from "./pages/MandatoryHITL";
import { Audit } from "./pages/Audit";
import { Spend } from "./pages/Spend";
import { TryAgent } from "./pages/TryAgent";
import { ErrorBoundary } from "./components/ErrorBoundary";

function Gate() {
  const { status } = useAuth();

  if (status === "checking") {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 bg-background text-foreground">
        <div className="flex size-11 items-center justify-center rounded-sm border border-primary bg-background font-mono text-xs font-bold tracking-wider text-primary">
          RK
        </div>
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" />
          Connecting to control plane...
        </div>
      </div>
    );
  }

  if (status === "unauthenticated") {
    return <Login />;
  }

  return (
    <Routes>
      <Route path="/admin" element={<Layout />}>
        <Route index element={<Overview />} />
        <Route path="agents" element={<Agents />} />
        <Route path="agents/:agentId" element={<AgentDetail />} />
        <Route path="registry" element={<Registry />} />
        {/* No separate "registry/new" route: it would match with no :name
            param at all (that pattern has no capture group of its own),
            so RegistryEntry's `routeName === "new"` check for isNew would
            never be true. Let "/admin/registry/new" fall through to the
            :name route instead, where routeName really is the literal
            string "new". */}
        <Route path="registry/:name" element={<RegistryEntry />} />
        <Route path="threads" element={<Threads />} />
        <Route path="threads/:threadId" element={<ThreadDetail />} />
        <Route path="runs" element={<Runs />} />
        <Route path="runs/:runId" element={<RunDetail />} />
        <Route path="connectors" element={<Connectors />} />
        <Route path="cron" element={<Cron />} />
        <Route path="webhooks" element={<Webhooks />} />
        <Route path="grants" element={<PolicyGrants />} />
        <Route path="mandatory-hitl" element={<MandatoryHITL />} />
        <Route path="pending" element={<PendingActions />} />
        <Route path="kill" element={<KillSwitches />} />
        <Route path="break-glass" element={<BreakGlass />} />
        <Route path="audit" element={<Audit />} />
        <Route path="spend" element={<Spend />} />
        <Route path="try" element={<TryAgent />} />
      </Route>
      <Route path="*" element={<Navigate to="/admin" replace />} />
    </Routes>
  );
}

export default function App() {
  return (
    <ErrorBoundary>
      <AuthProvider>
        <Gate />
      </AuthProvider>
    </ErrorBoundary>
  );
}
