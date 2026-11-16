import type { AgentProfileView } from "../lib/agentCapabilities";
import { supportPage } from "./common";

export function AgentCapabilities({ profile, compact = false }: { profile: AgentProfileView; compact?: boolean }) {
  return (
    <div className="space-y-3 text-sm">
      <div>
        <p className="font-medium">Configured profile: <code className="text-xs">{profile.framework}</code></p>
        <p className="mt-1 text-xs text-muted-foreground">{profile.provenance}</p>
        <p className="mt-1 text-xs text-muted-foreground">
          Advisory only. This does not grant permission or confirm a live runner, loaded code or isolation.
        </p>
      </div>
      <dl className="space-y-3">
        {profile.rows.map((row) => (
          <div key={row.key}>
            <dt className="text-xs text-muted-foreground">{row.label}</dt>
            <dd>
              <a className="text-primary hover:underline" href={supportPage(row.guide)} target="_blank" rel="noreferrer">
                {row.value}
              </a>
              {!compact && <p className="mt-0.5 text-xs text-muted-foreground">{row.detail}</p>}
            </dd>
          </div>
        ))}
      </dl>
      {compact && <p className="text-xs text-muted-foreground">Follow each guide for checkpoint and tool-policy conditions.</p>}
    </div>
  );
}
