package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func testApplicationMap(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var value map[string]interface{}
	if err := jsonvalue.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestApplicationJSONNativeBSONRoundTrip(t *testing.T) {
	raw := `{"id":9007199254740993,"negative":-9223372036854775808,"unsigned":18446744073709551615,"nested":[0.123456789123456789,{"wide":1234567890123456789012345678901234,"exponent":1e1000}],"zero":-0,"string":"9007199254740993"}`
	encoded, err := applicationJSONToBSON(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	document, err := bson.Marshal(bson.M{"value": encoded})
	if err != nil {
		t.Fatal(err)
	}
	decoder := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(document)))
	decoder.DefaultDocumentM()
	var decoded bson.M
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	native := decoded["value"].(bson.M)
	if _, ok := native["id"].(int64); !ok {
		t.Fatalf("large signed ID was not native int64: %T", native["id"])
	}
	if _, ok := native["unsigned"].(bson.Decimal128); !ok {
		t.Fatalf("uint64 was not exact native Decimal128: %T", native["unsigned"])
	}
	actual, err := applicationMap(native)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonvalue.Equal(testApplicationMap(t, raw), actual) {
		t.Fatalf("BSON changed application numbers: %#v", actual)
	}
	metadata := bsonToMap(bson.M{"generation": int64(3)})
	if metadata["generation"] != float64(3) {
		t.Fatalf("application codec changed control metadata: %#v", metadata)
	}
}

func TestApplicationJSONRejectsUnrepresentableNumbers(t *testing.T) {
	for _, raw := range []string{
		`{"id":12345678901234567890123456789012345}`,
		`{"id":1e10000}`,
		`{"id":1e-10000}`,
		`{"id":9007199254740993} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := applicationJSONToBSON(json.RawMessage(raw)); err == nil {
				t.Fatal("unsupported number was silently accepted")
			}
		})
	}
	for _, scalar := range []interface{}{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := applicationMap(bson.M{"bad": scalar}); err == nil {
			t.Fatal("non-finite BSON double was exposed as JSON")
		}
	}
	for _, raw := range []string{"NaN", "Infinity", "-Infinity"} {
		decimal, err := bson.ParseDecimal128(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := applicationMap(bson.M{"bad": decimal}); err == nil {
			t.Fatal("non-finite Decimal128 was exposed as JSON")
		}
	}
}

func TestApplicationJSONCanonicalizesLongCoefficientsBeforeNativeConversion(t *testing.T) {
	value := json.Number("1" + strings.Repeat("0", 100_000) + "e-100000")
	native, err := applicationNumberToBSON(value)
	if err != nil || native != int64(1) {
		t.Fatalf("compensated coefficient was not canonicalized before conversion: %T %v", native, err)
	}
	if _, err := applicationNumberToBSON(json.Number(strings.Repeat("1", 100_000))); err == nil {
		t.Fatal("unrepresentable long precision was accepted")
	}
}

func TestApplicationEqualityFilterAddsOnlySafeLegacyDouble(t *testing.T) {
	filter, err := applicationEqualityFilter(json.Number("0.1"))
	if err != nil {
		t.Fatal(err)
	}
	options, ok := filter.(bson.M)
	if !ok || len(options["$in"].(bson.A)) != 2 {
		t.Fatalf("safe decimal has no legacy fallback: %#v", filter)
	}
	if _, ok := options["$in"].(bson.A)[1].(float64); !ok {
		t.Fatalf("legacy fallback is not double: %#v", filter)
	}
	for _, value := range []json.Number{"9007199254740993", "0.123456789123456789"} {
		filter, err := applicationEqualityFilter(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, fallback := filter.(bson.M); fallback {
			t.Fatalf("inexact JSON number gained a legacy double alias: %#v", filter)
		}
	}
	operator := map[string]interface{}{"$gt": json.Number("0.1")}
	filter, err = applicationEqualityFilter(operator)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := filter.(map[string]interface{})["$in"]; changed {
		t.Fatal("numeric operator was changed into an equality query")
	}
}

func numericMongoStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		t.Skip("MONGO_URI is required for native numeric storage checks")
	}
	ctx := context.Background()
	store, err := New(ctx, uri, fmt.Sprintf("runkite_numbers_%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Drop(context.Background()); err != nil {
			t.Error(err)
		}
		store.Close()
	})
	return store
}

func numericMongoRun(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, Values: testApplicationMap(t, `{"retained":9007199254740993}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Input: json.RawMessage(`{"id":9007199254740993}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.BeginRunAttempt(ctx, "run", 1); err != nil || !accepted {
		t.Fatalf("begin attempt: %v %v", accepted, err)
	}
}

func TestMongoApplicationJSONNumbers(t *testing.T) {
	store := numericMongoStore(t)
	numericMongoRun(t, store)
	ctx := context.Background()
	values := testApplicationMap(t, `{"result":{"id":9007199254740993,"unsigned":18446744073709551615,"decimal":0.123456789123456789,"nested":[-9223372036854775808,1e1000]}}`)
	if completed, err := store.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess, Values: values}); err != nil || !completed.Applied {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || !jsonvalue.Equal(testApplicationMap(t, string(run.Output)), values) {
		t.Fatalf("run output changed: %+v %v", run, err)
	}
	if !jsonvalue.Equal(testApplicationMap(t, string(run.Input)), testApplicationMap(t, `{"id":9007199254740993}`)) {
		t.Fatalf("run input changed: %s", run.Input)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil || !jsonvalue.Equal(thread.Values["retained"], json.Number("9007199254740993")) || !jsonvalue.Equal(thread.Values["result"], values["result"]) {
		t.Fatalf("thread merge changed values: %+v %v", thread, err)
	}
	checkpoint, err := store.GetLatestCheckpoint(ctx, "thread")
	if err != nil || !jsonvalue.Equal(checkpoint.Values, values) {
		t.Fatalf("completion checkpoint changed values: %+v %v", checkpoint, err)
	}
	checkpoint.Checkpoint.CheckpointID = "explicit"
	checkpoint.Tasks = []interface{}{values}
	checkpoint.Interrupts = []interface{}{values}
	if err := store.SaveCheckpoint(ctx, "thread", checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := store.ListCheckpoints(ctx, "thread", 10, "")
	if err != nil || len(checkpoints) != 2 {
		t.Fatalf("checkpoint payload changed: %+v %v", checkpoints, err)
	}
	found := false
	for _, checkpoint := range checkpoints {
		if checkpoint.Checkpoint.CheckpointID == "explicit" {
			found = true
			if !jsonvalue.Equal(checkpoint.Tasks, []interface{}{values}) || !jsonvalue.Equal(checkpoint.Interrupts, []interface{}{values}) {
				t.Fatalf("checkpoint payload changed: %+v", checkpoint)
			}
		}
	}
	if !found {
		t.Fatal("explicit checkpoint missing")
	}
	if err := store.PutItem(ctx, &models.StoreItem{Namespace: []string{"numbers"}, Key: "item", Value: values}); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetItem(ctx, []string{"numbers"}, "item", false)
	if err != nil || !jsonvalue.Equal(item.Value, values) {
		t.Fatalf("store value changed: %+v %v", item, err)
	}
	items, err := store.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"numbers"}, Filter: map[string]interface{}{"result.id": json.Number("9007199254740993")}, Limit: 10})
	if err != nil || len(items) != 1 || !jsonvalue.Equal(items[0].Value, values) {
		t.Fatalf("exact store filter changed: %+v %v", items, err)
	}
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: "cache", AgentID: "agent", Output: values, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cached, err := store.GetCachedRunResult(ctx, "cache")
	if err != nil || !jsonvalue.Equal(cached.Output, values) {
		t.Fatalf("cached output changed: %+v %v", cached, err)
	}
}

func TestMongoUnsupportedApplicationNumberLeavesRunRecoverable(t *testing.T) {
	store := numericMongoStore(t)
	numericMongoRun(t, store)
	ctx := context.Background()
	values := testApplicationMap(t, `{"result":12345678901234567890123456789012345}`)
	if _, err := store.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess, Values: values}); err == nil {
		t.Fatal("unsupported completion was accepted")
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusRunning || run.Output != nil {
		t.Fatalf("rejected completion changed run: %+v %v", run, err)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil || len(thread.Values) != 1 || thread.Values["retained"] != json.Number("9007199254740993") {
		t.Fatalf("rejected completion changed thread: %+v %v", thread, err)
	}
	if _, err := store.GetLatestCheckpoint(ctx, "thread"); err == nil {
		t.Fatal("rejected completion wrote a checkpoint")
	}
	if err := store.PutItem(ctx, &models.StoreItem{Namespace: []string{"numbers"}, Key: "invalid", Value: values}); err == nil {
		t.Fatal("unsupported store item was accepted")
	}
	if _, err := store.GetItem(ctx, []string{"numbers"}, "invalid", false); err == nil {
		t.Fatal("unsupported store item was written")
	}
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: "invalid", Output: values}); err == nil {
		t.Fatal("unsupported cache output was accepted")
	}
	if _, err := store.GetCachedRunResult(ctx, "invalid"); err == nil {
		t.Fatal("unsupported cache output was written")
	}
}

func TestMongoAdmittedRunPreservesNumbersAndRejectsUnsupportedInput(t *testing.T) {
	store := numericMongoStore(t)
	numericMongoRun(t, store)
	ctx := context.Background()
	now := time.Now().UTC()
	caps := &state.RunAdmissionCaps{AgentConcurrent: 10}
	run := &models.Run{
		RunID: "admitted", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending,
		Input: json.RawMessage(`{"id":9007199254740993}`), Config: json.RawMessage(`{"id":18446744073709551615,"decimal":0.123456789123456789}`),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateRunAdmitted(ctx, run, caps); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonvalue.Equal(testApplicationMap(t, string(run.Input)), testApplicationMap(t, string(saved.Input))) || !jsonvalue.Equal(testApplicationMap(t, string(run.Config)), testApplicationMap(t, string(saved.Config))) {
		t.Fatalf("admission changed application data: input=%s config=%s", saved.Input, saved.Config)
	}
	for _, field := range []string{"input", "config"} {
		invalid := *run
		invalid.RunID = "invalid-" + field
		if field == "input" {
			invalid.Input = json.RawMessage(`{"id":12345678901234567890123456789012345}`)
		} else {
			invalid.Config = json.RawMessage(`{"id":12345678901234567890123456789012345}`)
		}
		if err := store.CreateRunAdmitted(ctx, &invalid, caps); err == nil {
			t.Fatalf("admission accepted unsupported %s", field)
		}
		if _, err := store.GetRun(ctx, invalid.RunID); err == nil {
			t.Fatalf("admission wrote unsupported %s", field)
		}
	}
}

func TestMongoApplicationEqualityMatchesLegacyDoubleWithoutIntegerAliases(t *testing.T) {
	store := numericMongoStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, legacy := range []storeItemDoc{
		{Key: "legacy-decimal", Value: bson.M{"value": float64(0.1)}},
		{Key: "legacy-rounded-integer", Value: bson.M{"value": float64(9007199254740992)}},
	} {
		legacy.TenantID = tenant.FromContext(ctx)
		legacy.Namespace = nsToString([]string{"numeric-filter"})
		legacy.CreatedAt, legacy.UpdatedAt = now, now
		if _, err := store.col("store_items").InsertOne(ctx, legacy); err != nil {
			t.Fatal(err)
		}
	}
	for _, exact := range []*models.StoreItem{
		{Namespace: []string{"numeric-filter"}, Key: "exact-decimal", Value: map[string]interface{}{"value": json.Number("0.1")}},
		{Namespace: []string{"numeric-filter"}, Key: "exact-integer", Value: map[string]interface{}{"value": json.Number("9007199254740993")}},
	} {
		if err := store.PutItem(ctx, exact); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"numeric-filter"}, Filter: map[string]interface{}{"value": json.Number("0.1")}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool)
	for _, item := range items {
		keys[item.Key] = true
	}
	if len(keys) != 2 || !keys["legacy-decimal"] || !keys["exact-decimal"] {
		t.Fatalf("decimal lookup missed legacy or exact storage: %v", keys)
	}
	items, err = store.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"numeric-filter"}, Filter: map[string]interface{}{"value": json.Number("9007199254740993")}, Limit: 10})
	if err != nil || len(items) != 1 || items[0].Key != "exact-integer" {
		t.Fatalf("large integer lookup matched rounded neighbour: %+v %v", items, err)
	}
}
