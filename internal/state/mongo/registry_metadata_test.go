package mongo

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRegistryMetadataRetainsLegacyJSONIngressAndNativeBSONTypes(t *testing.T) {
	var metadata map[string]interface{}
	if err := jsonvalue.Unmarshal([]byte(`{"id":9007199254740993,"nested":[1.0000000000000001,{"id":9007199254740993}]}`), &metadata); err != nil {
		t.Fatal(err)
	}
	decimal, err := bson.ParseDecimal128("1.0000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	metadata["native"] = bson.M{"id": int64(9007199254740993), "decimal": decimal}
	converted, err := registryMetadataForBSON(metadata)
	if err != nil {
		t.Fatal(err)
	}
	got := converted.(map[string]interface{})
	if got["id"] != float64(9007199254740992) || got["nested"].([]interface{})[0] != float64(1) {
		t.Fatalf("changed legacy HTTP JSON number semantics: %#v", got)
	}
	if _, ok := metadata["id"].(json.Number); !ok {
		t.Fatal("conversion mutated caller metadata")
	}
	encoded, err := bson.Marshal(bson.M{"metadata": got})
	if err != nil {
		t.Fatal(err)
	}
	var decoded bson.M
	decoder := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(encoded)))
	decoder.DefaultDocumentM()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	native := decoded["metadata"].(bson.M)["native"].(bson.M)
	if native["id"] != int64(9007199254740993) || native["decimal"] != decimal {
		t.Fatalf("changed native BSON scalar types: %#v", native)
	}
}

func TestRegistryMetadataConversionRejectsOverflowAndPreservesNull(t *testing.T) {
	if _, err := registryMetadataForBSON(map[string]interface{}{"nested": []interface{}{json.Number("1e400")}}); err == nil {
		t.Fatal("accepted a number outside the previous JSON float64 ingress range")
	}
	got, err := registryMetadataForBSON(map[string]interface{}(nil))
	if err != nil || got.(map[string]interface{}) != nil {
		t.Fatalf("null metadata changed: %#v %v", got, err)
	}
}
