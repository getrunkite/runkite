package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

type mcpSchemaStore struct {
	state.Store
	schema *models.AgentSchema
	err    error
	agents []*models.Agent
}

func (s *mcpSchemaStore) GetAgentSchema(context.Context, string) (*models.AgentSchema, error) {
	return s.schema, s.err
}

func (s *mcpSchemaStore) SearchAgents(context.Context, *models.AgentSearchRequest) ([]*models.Agent, error) {
	return s.agents, nil
}

func mcpSchemaObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var result map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func mcpSchemaJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mcpSchemaStatus(t *testing.T, meta mcp.Meta) map[string]any {
	t.Helper()
	status, ok := meta[mcpSchemaMetaKey].(map[string]any)
	if !ok || status["freshness"] != "unknown" || status["session_snapshot"] != true {
		t.Fatalf("missing advisory snapshot boundary: %#v", meta)
	}
	return status
}

func schemaPlane(input map[string]any) *Server {
	return &Server{store: &mcpSchemaStore{schema: &models.AgentSchema{InputSchema: input}}}
}

func TestMCPInputSchemaRebasesOnlySchemaReferencesAndDoesNotMutateSource(t *testing.T) {
	input := mcpSchemaObject(t, `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"$defs":{"escaped/~":{"type":"string"}},
		"properties":{
			"ticket":{"$ref":"#/$defs/escaped~1~0"},
			"child":{"$ref":"#"},
			"data":{"default":{"$ref":"#/$defs/keep-default","$id":"opaque-default"},
				"const":{"$ref":"#/$defs/keep-const"},
				"enum":[{"$ref":"#/$defs/keep-enum"}],
				"examples":[{"$ref":"https://example.invalid/user-data","dependencies":{"$ref":"#"}}]}
		},
		"required":["ticket"],
		"x-vendor":{"$ref":"#/$defs/keep-extension"}
	}`)
	before := mcpSchemaJSON(t, input)
	remaining := mcpSchemaCatalogLimit
	got, meta := schemaPlane(input).mcpInputSchema(context.Background(), "agent", &remaining)
	status := mcpSchemaStatus(t, meta)
	if status["status"] != "reported" || status["source"] != "agent_schema" {
		t.Fatalf("usable recursive schema fell back: %#v", status)
	}
	properties := got["properties"].(map[string]any)
	nested := properties["input"].(map[string]any)
	children := nested["properties"].(map[string]any)
	if children["ticket"].(map[string]any)["$ref"] != "#/properties/input/$defs/escaped~1~0" || children["child"].(map[string]any)["$ref"] != "#/properties/input" {
		t.Fatalf("local refs were not moved into input: %#v", children)
	}
	if _, exists := nested["$schema"]; exists {
		t.Fatal("nested dialect declaration was not normalized")
	}
	if !bytes.Equal(mcpSchemaJSON(t, children["data"]), mcpSchemaJSON(t, input["properties"].(map[string]any)["data"])) || !reflect.DeepEqual(nested["x-vendor"], input["x-vendor"]) {
		t.Fatal("instance-valued annotations were rewritten")
	}
	if !bytes.Equal(before, mcpSchemaJSON(t, input)) {
		t.Fatal("discovery mutated the stored schema")
	}
	digest := sha256.Sum256(before)
	if status["reported_schema_sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest does not identify the original reported schema: %#v", status)
	}
	for _, name := range []string{"message", "thread_id", "run_id"} {
		if !reflect.DeepEqual(properties[name], mcpToolInputSchema["properties"].(map[string]any)[name]) {
			t.Fatalf("discovery changed envelope field %q", name)
		}
	}
	if remaining != mcpSchemaCatalogLimit-len(mcpSchemaJSON(t, got)) {
		t.Fatal("accepted envelope was not charged to the catalog budget")
	}
	children["ticket"].(map[string]any)["$ref"] = "caller mutation"
	if !bytes.Equal(before, mcpSchemaJSON(t, input)) {
		t.Fatal("returned schema aliases the source")
	}
}

func TestMCPInputSchemaFallbackIsAlwaysFreshAndGeneric(t *testing.T) {
	valid := func() map[string]any {
		return mcpSchemaObject(t, `{"type":"object","properties":{"ticket":{"type":"string"}}}`)
	}
	tooLargeAfterEmbedding := valid()
	initialSize := len(mcpSchemaJSON(t, tooLargeAfterEmbedding))
	tooLargeAfterEmbedding["description"] = strings.Repeat("x", mcpInputSchemaLimit-initialSize-40)
	if len(mcpSchemaJSON(t, tooLargeAfterEmbedding)) > mcpInputSchemaLimit {
		t.Fatal("fixture must fit before embedding")
	}
	checkBudget := mcpSchemaBudget{}
	envelope := freshMCPToolEnvelope()
	envelope["properties"].(map[string]any)["input"] = tooLargeAfterEmbedding
	if !checkBudget.check(tooLargeAfterEmbedding, 0) || len(mcpSchemaJSON(t, envelope)) <= mcpInputSchemaLimit {
		t.Fatal("fixture must pass the source budget and fail only after embedding")
	}
	cases := []struct {
		name, raw, reason string
		input             map[string]any
		remaining         int
	}{
		{name: "stub", raw: `{"type":"object"}`, reason: "stub"},
		{name: "array root", raw: `{"type":"array","items":{"type":"string"}}`, reason: "unsupported_root"},
		{name: "root union", raw: `{"type":["object","null"],"properties":{"x":{"type":"string"}}}`, reason: "unsupported_root"},
		{name: "invalid schema after insertion", raw: `{"type":"object","properties":{"ticket":{"minimum":"invalid"}}}`, reason: "invalid_schema"},
		{name: "invalid regex during resolve", raw: `{"type":"object","properties":{"ticket":{"type":"string","pattern":"["}}}`, reason: "invalid_schema"},
		{name: "unresolved local ref", raw: `{"type":"object","properties":{"ticket":{"$ref":"#/$defs/missing"}}}`, reason: "invalid_schema"},
		{name: "remote ref", raw: `{"type":"object","properties":{"ticket":{"$ref":"https://example.invalid/schema"}}}`, reason: "unsupported_resource_or_reference"},
		{name: "file ref", raw: `{"type":"object","properties":{"ticket":{"$ref":"file:///private/schema.json"}}}`, reason: "unsupported_resource_or_reference"},
		{name: "anchor ref", raw: `{"type":"object","properties":{"ticket":{"$ref":"#named"}}}`, reason: "unsupported_resource_or_reference"},
		{name: "nested resource", raw: `{"type":"object","properties":{"ticket":{"$id":"nested","type":"string"}}}`, reason: "unsupported_resource_or_reference"},
		{name: "dynamic ref", raw: `{"type":"object","properties":{"ticket":{"$dynamicRef":"#"}}}`, reason: "unsupported_resource_or_reference"},
		{name: "old dialect", raw: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"ticket":{"type":"string"}}}`, reason: "unsupported_dialect"},
		{name: "legacy schema dependencies", raw: `{"type":"object","properties":{"ticket":{"dependencies":{"other":{"$ref":"#"}}}}}`, reason: "unsupported_legacy_keyword"},
		{name: "malformed schema child", raw: `{"type":"object","properties":{"ticket":7}}`, reason: "invalid_schema"},
		{name: "size after insertion", input: tooLargeAfterEmbedding, reason: "bounds_or_value_type"},
		{name: "catalog after insertion", input: valid(), remaining: 1, reason: "catalog_budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.input
			if input == nil {
				input = mcpSchemaObject(t, tc.raw)
			}
			before := mcpSchemaJSON(t, input)
			remaining := tc.remaining
			if remaining == 0 {
				remaining = mcpSchemaCatalogLimit
			}
			originalBudget := remaining
			got, meta := schemaPlane(input).mcpInputSchema(context.Background(), "agent", &remaining)
			status := mcpSchemaStatus(t, meta)
			if status["status"] != "generic" || status["reason"] != tc.reason || !reflect.DeepEqual(got, freshMCPToolEnvelope()) {
				t.Fatalf("unsafe fallback: schema=%#v meta=%#v", got, meta)
			}
			if remaining != originalBudget || !bytes.Equal(before, mcpSchemaJSON(t, input)) {
				t.Fatal("fallback changed budget or source")
			}
			got["properties"].(map[string]any)["message"].(map[string]any)["type"] = "caller mutation"
			if !reflect.DeepEqual(freshMCPToolEnvelope(), mcpToolInputSchema) {
				t.Fatal("generic fallback aliases the global envelope")
			}
		})
	}
}

func TestMCPInputSchemaBoundsAndUnavailableReports(t *testing.T) {
	hugeNumber := json.Number(strings.Repeat("1", mcpInputSchemaLimit+1))
	numberBudget := mcpSchemaBudget{}
	if numberBudget.check(hugeNumber, 0) {
		t.Fatal("number text escaped the pre-encoding byte budget")
	}
	deep := map[string]any{}
	for range 34 {
		deep = map[string]any{"nested": deep}
	}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	for _, value := range []any{strings.Repeat("x", mcpInputSchemaLimit+1), hugeNumber, make([]any, 4097), deep, cyclic, func() {}, math.NaN(), map[string]string{"unsupported": "typed map"}} {
		input := map[string]any{"type": "object", "properties": map[string]any{"ticket": map[string]any{"type": "string"}}, "default": value}
		got, meta := schemaPlane(input).mcpInputSchema(context.Background(), "agent", nil)
		if mcpSchemaStatus(t, meta)["status"] != "generic" || !reflect.DeepEqual(got, freshMCPToolEnvelope()) {
			t.Fatalf("unbounded or unsupported value %T was advertised", value)
		}
	}
	for _, fixture := range []*mcpSchemaStore{{}, {err: errors.New("storage unavailable")}, {schema: &models.AgentSchema{}}} {
		got, meta := (&Server{store: fixture}).mcpInputSchema(context.Background(), "agent", nil)
		if mcpSchemaStatus(t, meta)["reason"] != "missing_or_unavailable" || !reflect.DeepEqual(got, freshMCPToolEnvelope()) {
			t.Fatal("missing report did not retain generic invocation")
		}
	}
}

func connectMCPSchemaFixture(t *testing.T, ctx context.Context, plane *Server) *mcp.ClientSession {
	t.Helper()
	server, err := plane.newMCPServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "schema-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func listedMCPSchema(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string) (map[string]any, mcp.Meta) {
	t.Helper()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == name {
			return mcpSchemaObject(t, string(mcpSchemaJSON(t, tool.InputSchema))), tool.Meta
		}
	}
	t.Fatalf("tool %q was not advertised", name)
	return nil, nil
}

func TestMCPInputSchemaProtocolCatalogIsSessionSnapshot(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	setSchema := func(kind string) {
		t.Helper()
		input := map[string]any{"type": "object", "properties": map[string]any{"ticket": map[string]any{"type": kind}}}
		if err := f.store.UpsertAgentSchema(ctx, &models.AgentSchema{AgentID: "mcp-agent", InputSchema: input}); err != nil {
			t.Fatal(err)
		}
	}
	setSchema("string")
	firstSession := connectMCPSchemaFixture(t, ctx, f.plane)
	first, firstMeta := listedMCPSchema(t, ctx, firstSession, "mcp-agent")
	setSchema("integer")
	retained, retainedMeta := listedMCPSchema(t, ctx, firstSession, "mcp-agent")
	if !reflect.DeepEqual(first, retained) || !reflect.DeepEqual(firstMeta, retainedMeta) {
		t.Fatal("an existing session silently changed schema")
	}
	secondSession := connectMCPSchemaFixture(t, ctx, f.plane)
	second, secondMeta := listedMCPSchema(t, ctx, secondSession, "mcp-agent")
	if reflect.DeepEqual(first, second) || firstMeta[mcpSchemaMetaKey].(map[string]any)["reported_schema_sha256"] == secondMeta[mcpSchemaMetaKey].(map[string]any)["reported_schema_sha256"] {
		t.Fatal("a new session did not observe the changed report")
	}
	other, otherMeta := listedMCPSchema(t, ctx, secondSession, "other-agent")
	if mcpSchemaStatus(t, otherMeta)["status"] != "generic" || !bytes.Equal(mcpSchemaJSON(t, other), mcpSchemaJSON(t, freshMCPToolEnvelope())) {
		t.Fatal("an agent without a report borrowed another agent's schema")
	}
}

func TestMCPInputSchemaProtocolPreservesRawInputDefaultsAndRetry(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inputSchema := mcpSchemaObject(t, `{"type":"object","properties":{"ticket":{"type":"string","default":"AUTO"},"count":{"type":"integer"}},"required":["ticket"]}`)
	if err := f.store.UpsertAgentSchema(ctx, &models.AgentSchema{AgentID: "mcp-agent", InputSchema: inputSchema}); err != nil {
		t.Fatal(err)
	}
	session := connectMCPSchemaFixture(t, ctx, f.plane)
	_, meta := listedMCPSchema(t, ctx, session, "mcp-agent")
	if mcpSchemaStatus(t, meta)["status"] != "reported" {
		t.Fatal("test did not exercise enriched discovery")
	}
	rawInput := `{"count":"7","number":900719925474099312345678901234567890,"opaque":{"default":null}}`
	arguments := json.RawMessage(`{"run_id":"schema-raw-run","thread_id":"schema-raw-thread","message":"ignored because input wins","input":` + rawInput + `}`)
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp-agent", Arguments: arguments})
		done <- outcome{result, err}
	}()
	assignment, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("raw call was validated instead of dispatched: assignment=%#v err=%v", assignment, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, assignment.Input); err != nil || compact.String() != rawInput || assignment.ThreadID != "schema-raw-thread" || assignment.RunID != "schema-raw-run" {
		t.Fatalf("input/defaults/coercion or envelope changed: assignment=%#v input=%s err=%v", assignment, assignment.Input, err)
	}
	f.finish(t, ctx, assignment, models.RunStatusSuccess, "raw reply")
	first := <-done
	if first.err != nil || first.result.IsError || mcpRetryText(t, first.result) != "raw reply" {
		t.Fatalf("raw call failed: %+v", first)
	}
	retried, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp-agent", Arguments: arguments})
	if err != nil || retried.IsError || mcpRetryText(t, retried) != "raw reply" || f.queue.enqueued.Load() != 1 {
		t.Fatalf("typed discovery changed retry identity: result=%#v err=%v enqueued=%d", retried, err, f.queue.enqueued.Load())
	}
	conflict, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp-agent", Arguments: map[string]any{"run_id": "schema-raw-run", "input": map[string]any{"count": 7}}})
	if err != nil || !conflict.IsError || f.queue.enqueued.Load() != 1 {
		t.Fatalf("conflicting retry dispatched or escaped tool error: result=%#v err=%v", conflict, err)
	}
}

func TestMCPInputSchemaProtocolDiscoveryStaysTenantScoped(t *testing.T) {
	f := newMCPRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tenantID := range []string{"tenant-a", "tenant-b"} {
		scoped := tenant.WithContext(ctx, tenantID)
		if err := f.store.UpsertAgent(scoped, &models.Agent{AgentID: "same-agent", Name: "same-agent"}); err != nil {
			t.Fatal(err)
		}
		input := map[string]any{"type": "object", "properties": map[string]any{tenantID: map[string]any{"type": "string"}}}
		if err := f.store.UpsertAgentSchema(scoped, &models.AgentSchema{AgentID: "same-agent", InputSchema: input}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tenantID := range []string{"tenant-a", "tenant-b"} {
		scoped := tenant.WithContext(ctx, tenantID)
		session := connectMCPSchemaFixture(t, scoped, f.plane)
		schema, meta := listedMCPSchema(t, scoped, session, "same-agent")
		properties := schema["properties"].(map[string]any)["input"].(map[string]any)["properties"].(map[string]any)
		if mcpSchemaStatus(t, meta)["status"] != "reported" || len(properties) != 1 || properties[tenantID] == nil {
			t.Fatalf("schema crossed tenant boundary: tenant=%s schema=%#v", tenantID, schema)
		}
	}
}

func TestMCPInputSchemaProtocolSharesCatalogBudgetWithoutDroppingTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	input := map[string]any{
		"type": "object", "description": strings.Repeat("x", 40<<10),
		"properties": map[string]any{"ticket": map[string]any{"type": "string"}},
	}
	store := &mcpSchemaStore{schema: &models.AgentSchema{InputSchema: input}}
	for i := range 30 {
		store.agents = append(store.agents, &models.Agent{AgentID: fmt.Sprintf("catalog-agent-%02d", i)})
	}
	session := connectMCPSchemaFixture(t, ctx, &Server{store: store})
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != len(store.agents) {
		t.Fatalf("budget failure removed tools: tools=%#v err=%v", listed, err)
	}
	reported, generic, acceptedBytes := 0, 0, 0
	for _, tool := range listed.Tools {
		status := mcpSchemaStatus(t, tool.Meta)
		if status["status"] == "reported" {
			reported++
			acceptedBytes += len(mcpSchemaJSON(t, tool.InputSchema))
		} else {
			generic++
			if status["reason"] != "catalog_budget" || !bytes.Equal(mcpSchemaJSON(t, tool.InputSchema), mcpSchemaJSON(t, freshMCPToolEnvelope())) {
				t.Fatalf("catalog fallback retained an enriched shape: %#v", tool)
			}
		}
	}
	if reported == 0 || generic == 0 || acceptedBytes > mcpSchemaCatalogLimit {
		t.Fatalf("catalog budget was not shared: reported=%d generic=%d bytes=%d", reported, generic, acceptedBytes)
	}
}
