package mongo

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/state"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunObjectIDIsPrivateImmutableIdentity(t *testing.T) {
	id := bson.NewObjectID()
	doc := runDoc{ID: id, RunID: "run", ThreadID: "thread", TenantID: "tenant"}
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var stored runDoc
	if err := bson.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	run, err := toRun(stored)
	if err != nil {
		t.Fatal(err)
	}
	if run.IncarnationID != "" || state.RunIdentity(run) != "mongo-objectid:"+id.Hex() {
		t.Fatal("Mongo identity enabled a SQL public incarnation or lost the stored ObjectID")
	}
	public, err := json.Marshal(run)
	if err != nil || strings.Contains(string(public), id.Hex()) || strings.Contains(string(public), "StorageIdentity") {
		t.Fatal("private storage identity entered public run JSON", err)
	}
	for _, unsupported := range []interface{}{nil, bson.NilObjectID, id.Hex(), "custom", int64(123)} {
		doc.ID = unsupported
		if _, err := toRun(doc); err == nil {
			t.Fatalf("unsupported record identity type %T accepted", unsupported)
		}
	}
}
