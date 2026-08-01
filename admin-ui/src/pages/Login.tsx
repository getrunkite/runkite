import { useState, type FormEvent } from "react";
import { KeyRound, Loader2 } from "lucide-react";
import { useAuth } from "../auth/AuthContext";
import { ApiError } from "../api/client";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../components/ui/card";

export function Login() {
  const { login } = useAuth();
  const [token, setToken] = useState("");
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
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-background px-4">
      {/* Soft radial glow behind the card -- the kind of subtle depth
          that separates a "designed" auth screen from a bare form. */}
      <div
        className="pointer-events-none absolute inset-0 opacity-40"
        style={{
          background:
            "radial-gradient(600px circle at 50% 0%, var(--color-primary), transparent 60%)",
        }}
      />
      <Card className="relative w-full max-w-sm shadow-2xl">
        <CardHeader className="items-center text-center">
          <img
            src={`${import.meta.env.BASE_URL}logo.svg`}
            alt=""
            width={44}
            height={44}
            className="mb-2 size-11 rounded-xl shadow-lg shadow-primary/30"
          />
          <CardTitle className="text-lg">Runkite Admin</CardTitle>
          <CardDescription>
            Sign in with an API key or JWT that has the{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">admin</code> permission.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="token">API key / JWT</Label>
              <div className="relative">
                <KeyRound className="absolute top-1/2 left-3 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  id="token"
                  type="password"
                  autoFocus
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  placeholder="sk-... or eyJ..."
                  className="pl-9"
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
