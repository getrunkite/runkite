import { validManagedTargetRevision } from "../lib/pendingApproval";

export function ApprovalTargetRevision({ revision, standalone = false, compact = false }: {
  revision?: string;
  standalone?: boolean;
  compact?: boolean;
}) {
  if (standalone) return <span className="text-xs text-muted-foreground" title="Standalone decisions retain their app-owned execution and approval contract.">App-owned decision</span>;
  if (!validManagedTargetRevision(revision)) return <span className="text-xs text-amber-600 dark:text-amber-400" title="This managed approval requires a new submission and fresh review with a supported connector target revision.">No supported binding</span>;
  return <code className={compact ? "block max-w-48 truncate text-xs" : "break-all text-xs"} title={`${revision}. Stored configuration only; the server must verify the current target before approval and dispatch. This does not identify a tool schema or implementation.`}>{revision}</code>;
}
