import { useState, type FormEvent } from "react";
import { KeyRound, Loader2 } from "lucide-react";
import { useAuth } from "../auth/AuthContext";
import { ApiError } from "../api/client";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";
import { isPublicDemoHost, productHomeHref } from "../components/common";

/** Same shared key as the product page. Shown only on the public demo
 * host so a Sign out is not a dead end. */
const PUBLIC_SANDBOX_KEY = "600d9704de73d08db3b5d621b6e17c9172053cb480415ae8";

export function Login() {
  const { login } = useAuth();
  const demo = isPublicDemoHost();
  const [token, setToken] = useState(demo ? PUBLIC_SANDBOX_KEY : "");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login(token.trim());
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Login failed.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="relative flex min-h-screen items-center justify-center bg-background px-4">
      {demo && (
        <a
          href="/"
          className="absolute top-4 left-4 font-mono text-xs text-muted-foreground no-underline hover:text-primary"
        >
          ← Product
        </a>
      )}
      <Card className="relative w-full max-w-sm border-border">
        <CardHeader className="items-center text-center">
          <a href={productHomeHref()} className="mb-2 inline-flex no-underline">
            <img
              src={`${import.meta.env.BASE_URL}logo.svg`}
              alt=""
              width={44}
              height={44}
              className="size-11 rounded-sm"
            />
          </a>
          <CardTitle className="font-mono text-sm tracking-widest uppercase">
            ~/ <span className="text-primary">runkite</span> · Admin
          </CardTitle>
          <CardDescription>
            {demo
              ? "This is the live control plane, not a mock. Sign in with a credential that has the "
              : "Sign in with an API key or JWT that has the "}
            <code className="rounded-sm bg-muted px-1 py-0.5 font-mono text-xs">admin</code> permission.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            {demo && (
              <div className="space-y-2 rounded-sm border border-border bg-muted/40 px-3 py-2.5 text-left">
                <p className="text-xs font-medium text-foreground">Public sandbox key</p>
                <p className="break-all font-mono text-[11px] leading-snug text-muted-foreground">
                  {PUBLIC_SANDBOX_KEY}
                </p>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-full"
                  onClick={() => setToken(PUBLIC_SANDBOX_KEY)}
                >
                  Use sandbox key
                </Button>
              </div>
            )}
            <div className="space-y-1.5">
              <Label htmlFor="token">API key / JWT</Label>
              <div className="relative">
                <KeyRound className="absolute top-1/2 left-3 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  id="token"
                  type={demo ? "text" : "password"}
                  autoFocus
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  placeholder="sk-... or eyJ..."
                  className="pl-9 font-mono text-xs"
                />
              </div>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" disabled={submitting || token.trim() === ""} className="w-full">
              {submitting && <Loader2 className="size-4 animate-spin" />}
              {submitting ? "Signing in..." : "Sign in"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
