import { Component, type ErrorInfo, type ReactNode } from "react";

type Props = { children: ReactNode };
type State = { error: Error | null };

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("admin ui crashed", error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 bg-background px-6 text-foreground">
        <p className="font-medium">Admin UI hit an unexpected error.</p>
        <p className="max-w-md text-center text-sm text-muted-foreground">{this.state.error.message}</p>
        <button
          type="button"
          className="rounded-sm border border-border bg-card px-3 py-1.5 text-sm"
          onClick={() => window.location.reload()}
        >
          Reload
        </button>
      </div>
    );
  }
}
