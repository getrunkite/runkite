package conformance

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/google/uuid"
)

// RunActionDeliveries qualifies persistent metadata export without executing
// an upstream tool or relying on any external collector.
func RunActionDeliveries(t *testing.T, store state.Store) {
	deliveries, ok := store.(state.ActionDeliveryStore)
	if !ok {
		t.Fatal("action delivery store unavailable")
	}
	actions := store.(state.ProtectedActionStore)
	ctx, input := connectorActionFixture(t, store)
	system := tenant.SystemContext(ctx)
	if _, err := actions.CreateConnectorAction(ctx, input, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	rows, err := deliveries.SearchActionDeliveries(ctx, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("disabled export creates deliveries: %+v %v", rows, err)
	}
	input.ID = uuid.NewString()
	input.ExportSinkID = "conformance." + uuid.NewString()
	mutation := uuid.NewString()
	action, err := actions.CreateConnectorAction(ctx, input, mutation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := actions.CreateConnectorAction(ctx, input, mutation); err != nil {
		t.Fatal(err)
	}
	altered := *input
	altered.ExportSinkID += ".changed"
	if _, err := actions.CreateConnectorAction(ctx, &altered, mutation); err == nil {
		t.Fatal("mutation replay changed immutable sink")
	}
	decisionMutation := uuid.NewString()
	decision := models.ConnectorActionDecision{Effect: "allow", AuthorizationSource: "policy"}
	ready, err := actions.RecordConnectorActionDecision(ctx, action.ID, action.Version, decision, decisionMutation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := actions.RecordConnectorActionDecision(ctx, action.ID, action.Version, decision, decisionMutation); err != nil {
		t.Fatal(err)
	}
	rows, err = deliveries.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{SinkID: input.ExportSinkID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("event replay duplicated delivery: %+v %v", rows, err)
	}
	ids := map[string]bool{}
	for _, row := range rows {
		if ids[row.ID] || row.SinkID != input.ExportSinkID || row.TenantID != input.TenantID || row.ActionID != input.ID || row.Attempts != 0 || row.Status != state.ActionDeliveryPending || row.ClaimToken != "" || row.ClaimUntil != nil {
			t.Fatalf("wrong delivery metadata: %+v", row)
		}
		ids[row.ID] = true
		var payload state.ActionDeliveryPayload
		if err := json.Unmarshal(row.PayloadRaw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Schema != "runkite.action-event.v1" || payload.DeliveryID != row.ID || payload.SinkID != row.SinkID || payload.Action.ExportSinkID != row.SinkID || payload.Event.Sequence != row.Sequence || payload.Event.ActionID != input.ID {
			t.Fatalf("wrong export envelope: %+v", payload)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row.PayloadRaw, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 5 {
			t.Fatal("unexpected unbounded export fields")
		}
		raw := string(row.PayloadRaw)
		for _, forbidden := range []string{`"args":`, `"arguments":`, `"payload":`, `"claim_token":`, `"endpoint":`, `"token":`, `"password":`} {
			if strings.Contains(raw, forbidden) {
				t.Fatal("export envelope includes forbidden raw field")
			}
		}
	}
	foreign := tenant.ScopedContext(ctx, input.TenantID+"-foreign")
	if rows, err := deliveries.SearchActionDeliveries(foreign, &state.ActionDeliverySearchRequest{TenantID: input.TenantID}); err != nil || len(rows) != 0 {
		t.Fatalf("foreign export visible: %+v %v", rows, err)
	}
	if _, err := deliveries.ClaimActionDeliveries(ctx, input.ExportSinkID, time.Now(), time.Now().Add(time.Minute), 1); err == nil {
		t.Fatal("tenant context claimed global worker authority")
	}
	if _, err := deliveries.ClaimActionDeliveries(system, input.ExportSinkID, time.Now(), time.Now().Add(time.Minute), 101); err == nil {
		t.Fatal("unbounded batch accepted")
	}
	// Two workers race for the same two rows. Each lease has one current owner.
	var wg sync.WaitGroup
	claimed := make(chan *state.ActionDelivery, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now().UTC()
			batch, err := deliveries.ClaimActionDeliveries(system, input.ExportSinkID, now, now.Add(time.Minute), 1)
			if err != nil {
				t.Error(err)
				return
			}
			for _, row := range batch {
				claimed <- row
			}
		}()
	}
	wg.Wait()
	close(claimed)
	leases := []*state.ActionDelivery{}
	for row := range claimed {
		leases = append(leases, row)
	}
	// MySQL's next-key locks can temporarily hide an otherwise available row
	// from all overlapping SKIP LOCKED scans. Once those transactions finish,
	// the next worker tick must claim every remaining row without duplication.
	now := time.Now().UTC()
	remaining, err := deliveries.ClaimActionDeliveries(system, input.ExportSinkID, now, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, remaining...)
	seen := map[string]bool{}
	for _, row := range leases {
		if seen[row.ID] {
			t.Fatal("concurrent claims shared a delivery")
		}
		seen[row.ID] = true
		if row.ClaimToken == "" || row.Attempts != 1 {
			t.Fatal("claim receipt missing authority or attempt")
		}
	}
	if len(leases) != 2 {
		t.Fatalf("claimed=%d want2", len(leases))
	}
	first, second := leases[0], leases[1]
	if err := deliveries.AckActionDelivery(ctx, first.ID, first.ClaimToken); err == nil {
		t.Fatal("tenant caller acknowledged worker claim")
	}
	if err := deliveries.AckActionDelivery(system, first.ID, uuid.NewString()); err == nil {
		t.Fatal("wrong claim token acknowledged delivery")
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), first.ClaimToken) {
		t.Fatal("claim authority leaked through JSON")
	}
	if err := deliveries.AckActionDelivery(system, first.ID, first.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if err := deliveries.AckActionDelivery(system, first.ID, first.ClaimToken); err == nil {
		t.Fatal("stale acknowledgement succeeded")
	}
	// Simulate a later worker after expiry without sleeping. Payload and delivery
	// identity are unchanged, so receivers can deduplicate a lost acknowledgement.
	later := time.Now().UTC().Add(2 * time.Minute)
	recovered, err := deliveries.ClaimActionDeliveries(system, input.ExportSinkID, later, later.Add(time.Minute), 10)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("expired lease recovery: %+v %v", recovered, err)
	}
	current := recovered[0]
	if current.ID != second.ID || current.ClaimToken == second.ClaimToken || current.Attempts != 2 || string(current.PayloadRaw) != string(second.PayloadRaw) {
		t.Fatal("lease recovery changed delivery identity/payload")
	}
	if err := deliveries.AckActionDelivery(system, second.ID, second.ClaimToken); err == nil {
		t.Fatal("superseded lease acknowledged")
	}
	if err := deliveries.FailActionDelivery(system, current.ID, current.ClaimToken, time.Now(), "https://provider.invalid/?token=sentinel", true); err == nil {
		t.Fatal("raw provider error accepted as reason code")
	}
	if err := deliveries.FailActionDelivery(system, current.ID, current.ClaimToken, time.Now(), "http_status", true); err != nil {
		t.Fatal(err)
	}
	dead, err := deliveries.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{Status: state.ActionDeliveryDeadLetter, SinkID: input.ExportSinkID})
	if err != nil || len(dead) != 1 || dead[0].ID != current.ID || dead[0].LastReasonCode != "http_status" || dead[0].ClaimToken != "" {
		t.Fatalf("dead letter metadata: %+v %v", dead, err)
	}
	if err := deliveries.RetryActionDelivery(foreign, input.TenantID, current.ID); err == nil {
		t.Fatal("foreign tenant retried delivery")
	}
	if err := deliveries.RetryActionDelivery(ctx, input.TenantID, current.ID); err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC()
	retry, err := deliveries.ClaimActionDeliveries(system, input.ExportSinkID, now, now.Add(time.Minute), 10)
	if err != nil || len(retry) != 1 || retry[0].ID != current.ID || retry[0].Attempts != 3 {
		t.Fatalf("dead letter retry: %+v %v", retry, err)
	}
	if err := deliveries.FailActionDelivery(system, retry[0].ID, retry[0].ClaimToken, time.Now().Add(time.Hour), "timeout", false); err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC()
	if early, err := deliveries.ClaimActionDeliveries(system, input.ExportSinkID, now, now.Add(time.Minute), 10); err != nil || len(early) != 0 {
		t.Fatalf("backoff ignored: %+v %v", early, err)
	}
	before, err := actions.GetConnectorAction(ctx, ready.ID)
	if err != nil || before.State != models.ConnectorActionReady || before.Version != 2 {
		t.Fatal("export retry changed tool authority")
	}
	// Retaining the export independently permits investigating delivery after
	// ordinary action retention. No tool dispatch is involved in this fixture.
	if _, err := actions.FinishConnectorAction(ctx, ready.ID, ready.Version, "", models.ConnectorActionObservation{State: models.ConnectorActionNotDispatched}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if count, err := actions.PruneConnectorActions(ctx, time.Now().Add(time.Hour), 100); err != nil || count < 1 {
		t.Fatalf("action prune: %d %v", count, err)
	}
	retained, err := deliveries.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{SinkID: input.ExportSinkID})
	if err != nil || len(retained) != 3 {
		t.Fatalf("action retention erased delivery: %+v %v", retained, err)
	}
	page, err := deliveries.SearchActionDeliveries(ctx, &state.ActionDeliverySearchRequest{SinkID: input.ExportSinkID, Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != retained[1].ID {
		t.Fatalf("offset page: %+v %v", page, err)
	}
	for _, req := range []*state.ActionDeliverySearchRequest{{Limit: 101}, {Offset: -1}, {Offset: 10001}, {Status: "unknown"}} {
		if _, err := deliveries.SearchActionDeliveries(ctx, req); err == nil {
			t.Fatal("invalid export search accepted")
		}
	}
}
