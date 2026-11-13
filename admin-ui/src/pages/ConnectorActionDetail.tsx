import { ConnectorActionRecord } from "../components/connector-action-record";
import { Link, useParams } from "react-router";
import type { AdminConnectorActionDetail } from "../api/types";
import { useConnectorEvidence } from "../api/useConnectorEvidence";
import {
  connectorActionPath,
  connectorActionRunHref,
  connectorActionsHref,
  connectorApprovalQueueHref,
} from "../lib/connectorActions";
import { EvidenceRefresh } from "../components/connector-action-evidence";
import { DocsLink, ErrorState, PageHeader, supportPage } from "../components/common";
import { Skeleton } from "../components/ui/skeleton";

export function ConnectorActionDetail() {
  const { actionId } = useParams<{ actionId: string }>();
  return actionId ? (
    <ActionDetail key={actionId} actionId={actionId} />
  ) : (
    <ErrorState message="No connector action ID was provided." />
  );
}


function ActionDetail({ actionId }: { actionId: string }) {
  const history = useConnectorEvidence<AdminConnectorActionDetail>(connectorActionPath(actionId));
  if (history.loading && !history.data)
    return (
      <div className="space-y-4">
        <Skeleton className="h-16 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  if (!history.data)
    return (
      <div className="space-y-4">
        <Link to="/admin/connector-actions" className="text-primary hover:underline">
          ← Connector actions
        </Link>
        <ErrorState message={history.error || "No connector action was returned."} />
        <EvidenceRefresh refresh={history.reload} loading={history.loading} />
      </div>
    );
  const { action, run_available } = history.data;
  const runHref = connectorActionRunHref(action, run_available);
  const pendingHref = connectorApprovalQueueHref(action.tenant_id, action.run_id);
  return (
    <div className="space-y-4">
      <PageHeader
        title="Connector action"
        subtitle={
          <>
            <Link
              to={connectorActionsHref({ tenant_id: action.tenant_id, run_id: action.run_id })}
              className="text-primary hover:underline"
            >
              ← Retained history
            </Link>
            <span className="mx-2">·</span>
            <code className="break-all text-xs">{action.action_id}</code>
          </>
        }
        actions={
          <>
            <DocsLink href={supportPage("connector-actions.html")}>Docs: connector actions →</DocsLink>
            <EvidenceRefresh refresh={history.reload} loading={history.loading} />
          </>
        }
      />
      {history.error && <ErrorState message={history.error} />}
      <ConnectorActionRecord data={history.data} runHref={runHref} pendingHref={pendingHref} />
    </div>
  );
}
