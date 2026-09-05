import { Link, useParams, useSearchParams } from "react-router";
import { FlaskConical } from "lucide-react";
import { useApi } from "../api/useApi";
import type { AdminAgent, AdminAgentSchema, AdminAgentVersion } from "../api/types";
import { DocsLink, ErrorState, formatTimestamp, PageHeader, supportPage } from "../components/common";
import { adminAgentPath, adminAgentSchemasPath, adminAgentVersionsPath } from "../lib/adminPaths";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { Skeleton } from "../components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../components/ui/table";

function JsonBlock({ value }: { value: unknown }) {
  return (
    <pre className="max-h-64 overflow-auto rounded-lg bg-muted/50 p-3 font-mono text-xs">
      {JSON.stringify(value ?? {}, null, 2)}
    </pre>
  );
}

export function AgentDetail() {
  const { agentId = "" } = useParams<{ agentId: string }>();
  const [searchParams] = useSearchParams();
  const tenantId = searchParams.get("tenant_id") ?? undefined;
  const agent = useApi<AdminAgent>(adminAgentPath(agentId, tenantId), [agentId, tenantId]);
  const schema = useApi<AdminAgentSchema>(adminAgentSchemasPath(agentId, tenantId), [agentId, tenantId]);
  const versions = useApi<AdminAgentVersion[]>(adminAgentVersionsPath(agentId, tenantId), [agentId, tenantId]);

  if (agent.loading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }
  if (agent.error) return <ErrorState message={agent.error} />;
  if (!agent.data) return null;

  const a = agent.data;
  const tryHref = `/admin/try?tenant_id=${encodeURIComponent(a.tenant_id)}&agent_id=${encodeURIComponent(a.agent_id)}`;

  return (
    <div>
      <PageHeader
        title={a.name || a.agent_id}
        subtitle={`Registered agent · v${a.version}`}
        actions={<DocsLink href={supportPage("agents.html")}>Docs: agents →</DocsLink>}
      />

      <div className="mb-4 flex flex-wrap gap-2">
        <Button variant="outline" size="sm" asChild>
          <Link to="/admin/agents">← All agents</Link>
        </Button>
        <Button variant="outline" size="sm" asChild>
          <Link to={tryHref}>
            <FlaskConical className="size-3.5" />
            Try in playground
          </Link>
        </Button>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Identity</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3 text-sm">
            <div className="flex justify-between gap-4">
              <span className="text-muted-foreground">Agent ID</span>
              <span className="font-mono text-xs">{a.agent_id}</span>
            </div>
            <div className="flex justify-between gap-4">
              <span className="text-muted-foreground">Tenant</span>
              <Badge variant="outline">{a.tenant_id}</Badge>
            </div>
            <div className="flex justify-between gap-4">
              <span className="text-muted-foreground">Version</span>
              <span>v{a.version}</span>
            </div>
            {a.description ? (
              <div>
                <span className="text-muted-foreground">Description</span>
                <p className="mt-1">{a.description}</p>
              </div>
            ) : null}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Metadata</CardTitle>
          </CardHeader>
          <CardContent>
            <JsonBlock value={a.metadata} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Capabilities</CardTitle>
          </CardHeader>
          <CardContent>
            <JsonBlock value={a.capabilities} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Input / output schema</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {schema.error ? (
              <p className="text-sm text-muted-foreground">{schema.error}</p>
            ) : schema.loading ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <>
                <div>
                  <p className="mb-1 text-xs font-medium text-muted-foreground">Input</p>
                  <JsonBlock value={schema.data?.input_schema} />
                </div>
                <div>
                  <p className="mb-1 text-xs font-medium text-muted-foreground">Output</p>
                  <JsonBlock value={schema.data?.output_schema} />
                </div>
              </>
            )}
            <p className="text-xs text-muted-foreground">
              Schemas come from the runner after graph load, or a bootstrap stub until then. This page is
              a read-only catalog — editing graphs from Admin is not yet supported.
            </p>
          </CardContent>
        </Card>
      </div>

      <Card className="mt-4">
        <CardHeader>
          <CardTitle className="text-base">Version history</CardTitle>
        </CardHeader>
        <CardContent>
          {versions.error ? (
            <p className="text-sm text-muted-foreground">{versions.error}</p>
          ) : versions.loading ? (
            <Skeleton className="h-24 w-full" />
          ) : !versions.data?.length ? (
            <p className="text-sm text-muted-foreground">No version snapshots yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Version</TableHead>
                  <TableHead>Name</TableHead>
                  <TableHead>Tenant</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {versions.data.map((v) => (
                  <TableRow key={`${v.tenant_id}:${v.version}`}>
                    <TableCell>v{v.version}</TableCell>
                    <TableCell>{v.name}</TableCell>
                    <TableCell>
                      <Badge variant="outline">{v.tenant_id}</Badge>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{formatTimestamp(v.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
