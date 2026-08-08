import { Navigate, Route, Routes } from "react-router";
import { Loader2, Rocket } from "lucide-react";
import { AuthProvider, useAuth } from "./auth/AuthContext";
import { Layout } from "./components/Layout";
import { Login } from "./pages/Login";
import { Overview } from "./pages/Overview";
import { Agents } from "./pages/Agents";
import { Registry } from "./pages/Registry";
import { Threads } from "./pages/Threads";
import { ThreadDetail } from "./pages/ThreadDetail";
import { Runs } from "./pages/Runs";
import { RunDetail } from "./pages/RunDetail";
import { Connectors } from "./pages/Connectors";
import { Cron } from "./pages/Cron";
import { Webhooks } from "./pages/Webhooks";
import { PolicyGrants } from "./pages/PolicyGrants";
import { PendingActions } from "./pages/PendingActions";
import { Audit } from "./pages/Audit";

function Gate() {
  const { status } = useAuth();

  if (status === "checking") {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 bg-background text-foreground">
        <div className="flex size-11 items-center justify-center rounded-xl bg-primary text-primary-foreground shadow-lg shadow-primary/30">
          <Rocket className="size-5" />
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
        <Route path="registry" element={<Registry />} />
        <Route path="threads" element={<Threads />} />
        <Route path="threads/:threadId" element={<ThreadDetail />} />
        <Route path="runs" element={<Runs />} />
        <Route path="runs/:runId" element={<RunDetail />} />
        <Route path="connectors" element={<Connectors />} />
        <Route path="cron" element={<Cron />} />
        <Route path="webhooks" element={<Webhooks />} />
        <Route path="grants" element={<PolicyGrants />} />
        <Route path="pending" element={<PendingActions />} />
        <Route path="audit" element={<Audit />} />
      </Route>
      <Route path="*" element={<Navigate to="/admin" replace />} />
    </Routes>
  );
}

export default function App() {
  return (
    <AuthProvider>
      <Gate />
    </AuthProvider>
  );
}
