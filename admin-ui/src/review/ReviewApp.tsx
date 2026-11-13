import { useState, type FormEvent } from "react";
import { Link, NavLink, Navigate, Outlet, Route, Routes } from "react-router";
import { Loader2, LogOut, ShieldCheck } from "lucide-react";
import { ReviewProvider } from "./ReviewContext";
import { useReview } from "./context";
import { reviewClient } from "./client";
import { REVIEW_HELP, REVIEW_ROOT } from "./contracts";
import { ReviewCredentialLeases, ReviewCredentialLeaseDetail } from "./ReviewCredentialLeases";
import { ReviewEvidenceDetail, ReviewList, ReviewPendingDetail } from "./ReviewPages";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { ErrorState } from "../components/common";

export function ReviewApp() {
  return <ReviewProvider><ReviewGate /></ReviewProvider>;
}

function ReviewGate() {
  const review = useReview();
  if (!review.checked) return <div className="flex min-h-screen items-center justify-center gap-2"><Loader2 className="size-4 animate-spin" />Checking Review session...</div>;
  if (!review.identity) return <ReviewLogin />;
  return <div key={review.revision}><Routes>
    <Route path={REVIEW_ROOT} element={<ReviewLayout />}>
      <Route index element={<ReviewList kind="connector-actions" />} />
      <Route path="connector-actions/:actionId" element={<ReviewEvidenceDetail />} />
      <Route path="pending" element={<ReviewList kind="pending-actions" />} />
      <Route path="pending/:pendingId" element={<ReviewPendingDetail />} />
      <Route path="credential-leases" element={<ReviewCredentialLeases />} />
      <Route path="credential-leases/:leaseRef" element={<ReviewCredentialLeaseDetail />} />
      <Route path="*" element={<Navigate to={REVIEW_ROOT} replace />} />
    </Route>
  </Routes></div>;
}

function ReviewLogin() {
  const review = useReview();
  const [credential, setCredential] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true); setError(null);
    const once = credential.trim();
    setCredential("");
    try { await reviewClient.login(once); }
    catch (err) { setError(err instanceof Error ? err.message : "Review sign-in failed."); }
    finally { setBusy(false); }
  };
  return <main className="flex min-h-screen items-center justify-center bg-background p-4">
    <Card className="w-full max-w-md">
      <CardHeader><CardTitle className="flex items-center gap-2"><ShieldCheck className="size-5 text-primary" />Runkite Review</CardTitle></CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm text-muted-foreground">Review connector evidence and pending approvals for your verified tenant. Your credential is exchanged once for a separate session lasting at most 15 minutes.</p>
        {!review.configured && <ErrorState message="Review authentication is not configured. Ask the operator to configure the primary authentication provider and issue a credential with an explicit identity, tenant and governance:read or governance:approve permission. Global Admin keys do not open Review." />}
        {(error || review.error) && <ErrorState message={error || review.error!} />}
        <form onSubmit={submit} className="space-y-3">
          <Label htmlFor="review-credential">Review API key or JWT</Label>
          <Input id="review-credential" type="password" autoComplete="off" value={credential} onChange={(event) => setCredential(event.target.value)} disabled={busy || !review.configured} />
          <Button className="w-full" disabled={busy || !review.configured || !credential.trim()}>{busy ? "Signing in..." : "Sign in to Review"}</Button>
        </form>
        <div className="flex items-center justify-between text-sm"><a href={REVIEW_HELP} target="_blank" rel="noopener noreferrer" className="text-primary hover:underline">Review setup and permissions →</a><Button variant="ghost" size="sm" onClick={review.retry}>Check again</Button></div>
        <p className="text-sm text-muted-foreground">Deployment-wide administrators can use the <a href="/admin/" className="text-primary hover:underline">Admin console</a>.</p>
      </CardContent>
    </Card>
  </main>;
}

function ReviewLayout() {
  const { identity } = useReview();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  if (!identity) return null;
  const logout = async () => {
    setBusy(true); setError(null);
    try { await reviewClient.logout(); }
    catch (err) { setError(err instanceof Error ? err.message : "Sign out was not confirmed. Try again."); }
    finally { setBusy(false); }
  };
  return <div className="min-h-screen bg-background text-foreground">
    <a className="sr-only focus:not-sr-only" href="#review-main">Skip to content</a>
    <header className="border-b border-border"><div className="mx-auto flex max-w-7xl flex-wrap items-center justify-between gap-4 px-4 py-4 sm:px-6">
      <Link to={REVIEW_ROOT} className="flex items-center gap-2 font-mono text-sm font-semibold"><ShieldCheck className="size-5 text-primary" />RUNKITE <span className="text-muted-foreground">/ Review</span></Link>
      <nav aria-label="Review navigation" className="flex flex-wrap items-center gap-4 text-sm">
        <NavLink end to={REVIEW_ROOT} className={({ isActive }) => isActive ? "text-primary underline underline-offset-4" : "hover:text-primary"}>Evidence</NavLink>
        <NavLink to={`${REVIEW_ROOT}/pending`} className={({ isActive }) => isActive ? "text-primary underline underline-offset-4" : "hover:text-primary"}>Approvals</NavLink>
        <NavLink to={`${REVIEW_ROOT}/credential-leases`} className={({ isActive }) => isActive ? "text-primary underline underline-offset-4" : "hover:text-primary"}>Credential leases</NavLink>
        <a href={REVIEW_HELP} target="_blank" rel="noopener noreferrer" className="hover:text-primary">Help ↗</a>
        <Button size="sm" variant="ghost" disabled={busy} onClick={logout}><LogOut className="size-4" />Sign out</Button>
      </nav>
    </div></header>
    <div className="mx-auto max-w-7xl px-4 py-3 text-xs text-muted-foreground sm:px-6">
      <p>Verified tenant: <strong className="break-all text-foreground">{identity.tenant_id}</strong> · {identity.identity} · {identity.can_approve ? "Evidence and approval review" : "Read-only evidence and approvals"}</p>
      <p className="mt-1">Session ends {new Date(identity.expires_at).toLocaleString()}. Conversation contents and global administration are outside this workspace.</p>
      {error && <div className="mt-3"><ErrorState message={error} /></div>}
    </div>
    <main id="review-main" className="mx-auto max-w-7xl px-4 py-5 sm:px-6"><Outlet /></main>
  </div>;
}
