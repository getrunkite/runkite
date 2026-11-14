import { isolatedUsageView } from "../lib/isolatedUsage";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";
import { EvidenceField } from "./connector-action-record";

export function IsolatedUsageRecord({ output }: { output: unknown }) {
  const usage = isolatedUsageView(output);
  if (!usage) return null;
  return <Card className="mb-4"><CardHeader><CardTitle>Isolated workload usage</CardTitle></CardHeader><CardContent>
    <dl className="space-y-3 text-sm">
      <EvidenceField label="Report state">{usage.status === "unknown" ? "Unavailable" : usage.status === "partial" ? "Partial workload report" : "Workload declared complete"}</EvidenceField>
      <EvidenceField label="Recorded tokens">{usage.tokens}</EvidenceField>
      <EvidenceField label="Reported USD">{usage.dollars}</EvidenceField>
      <EvidenceField label="Committed sequence">{usage.sequence}</EvidenceField>
      {usage.reporter && <EvidenceField label="Submitting workload">{usage.reporter}</EvidenceField>}
    </dl>
    <p className="mt-4 text-xs text-muted-foreground">Latest acknowledged report settled with this run. Measurements are workload claims and have not been verified against provider billing. Partial or unavailable usage can omit activity. Later reports cannot rewrite a terminal settlement.</p>
  </CardContent></Card>;
}
