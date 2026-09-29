import { Link } from "react-router";
import { hopPhase, hopToolLabel, pendingPageHref, type HopPhase } from "../lib/runHop";

function trunc(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n - 1)}…`;
}

const PHASE_COPY: Record<HopPhase, string> = {
  idle: "This run's hop. Live from the stream, not a canvas.",
  running: "In flight.",
  held: "Plane held the tool. Click the tool hop for Pending.",
  denied: "Plane denied the tool.",
};

export function RunHop({
  agentId,
  tenantId,
  runId,
  busy,
  connectorPending,
  connector,
  tool,
  effect,
}: {
  agentId: string;
  tenantId: string;
  runId: string | null;
  busy: boolean;
  connectorPending: boolean;
  connector: string | null;
  tool: string | null;
  effect: string | null;
}) {
  const phase = hopPhase({ busy, connectorPending, effect });
  const toolName = hopToolLabel({ agentId, connector, tool });
  const href = pendingPageHref({ runId, tenantId });
  const held = phase === "held";
  const denied = phase === "denied";
  const running = phase === "running";
  const toolFill = "var(--card)";
  const toolStroke = held ? "var(--warning)" : denied ? "var(--destructive)" : "var(--border)";

  return (
    <div className="rounded-sm border border-border bg-card px-3 py-2">
      <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Hop</p>
      <div className="relative mt-1">
        <svg viewBox="0 0 360 72" className="h-20 w-full" role="img" aria-label="Agent to plane to tool">
          <path
            d="M 92 36 C 118 36, 118 36, 132 36"
            fill="none"
            stroke="var(--border)"
            strokeWidth="1.5"
          />
          <path
            d="M 228 36 C 242 36, 242 36, 256 36"
            fill="none"
            stroke="var(--border)"
            strokeWidth="1.5"
          />
          {running ? (
            <>
              {/*
                animateMotion maps the circle origin onto the path, so cx/cy
                stay 0,0. A delayed circle is invisible until begin so it
                does not flash at the SVG origin before it joins its wire.
              */}
              <circle r="3" cx="0" cy="0" fill="var(--primary)">
                <animateMotion dur="1.2s" repeatCount="indefinite" path="M 92 36 C 118 36, 118 36, 132 36" />
              </circle>
              <circle r="3" cx="0" cy="0" fill="var(--primary)" opacity="0">
                <set attributeName="opacity" to="1" begin="0.4s" />
                <animateMotion dur="1.2s" begin="0.4s" repeatCount="indefinite" path="M 228 36 C 242 36, 242 36, 256 36" />
              </circle>
            </>
          ) : null}
          <rect x="4" y="10" width="88" height="52" rx="2" fill="var(--card)" stroke="var(--border)" />
          <text x="48" y="28" textAnchor="middle" fill="var(--muted-foreground)" fontSize="8">
            agent
          </text>
          <text x="48" y="46" textAnchor="middle" fill="var(--foreground)" fontSize="10" fontFamily="ui-monospace, monospace">
            {trunc(agentId || "agent", 12)}
          </text>
          <rect x="136" y="10" width="88" height="52" rx="2" fill="var(--card)" stroke="var(--border)" />
          <text x="180" y="28" textAnchor="middle" fill="var(--muted-foreground)" fontSize="8">
            plane
          </text>
          <text x="180" y="46" textAnchor="middle" fill="var(--foreground)" fontSize="10" fontFamily="ui-monospace, monospace">
            hold / kill
          </text>
          <rect x="268" y="10" width="88" height="52" rx="2" fill={toolFill} stroke={toolStroke} strokeWidth={held ? 2 : 1} />
          <text x="312" y="28" textAnchor="middle" fill="var(--muted-foreground)" fontSize="8">
            tool
          </text>
          <text x="312" y="46" textAnchor="middle" fill="var(--foreground)" fontSize="10" fontFamily="ui-monospace, monospace">
            {trunc(toolName, 12)}
          </text>
        </svg>
        {held ? (
          <Link
            to={href}
            className="absolute inset-y-0 right-0 w-[31%] rounded-sm"
            aria-label="Open Pending for this run"
          />
        ) : null}
      </div>
      <p className="mt-1 text-xs text-muted-foreground">
        {PHASE_COPY[phase]}{" "}
        {held ? (
          <Link className="font-medium text-primary hover:underline" to={href}>
            Pending
          </Link>
        ) : null}
      </p>
    </div>
  );
}
