package mongo

import (
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Registry HTTP ingress retains number tokens for SQL. Mongo control metadata
// keeps its existing JSON float64 ingress semantics. Convert only json.Number
// leaves before BSON encoding; native BSON values from direct callers keep their
// existing types. This is not the exact application JSON/Decimal128 codec.
func registryMetadataForBSON(value interface{}) (interface{}, error) {
	switch value := value.(type) {
	case json.Number:
		number, err := value.Float64()
		if err != nil {
			return nil, fmt.Errorf("registry metadata number exceeds Mongo JSON ingress range")
		}
		return number, nil
	case map[string]interface{}:
		if value == nil {
			return value, nil
		}
		out := make(map[string]interface{}, len(value))
		for key, child := range value {
			converted, err := registryMetadataForBSON(child)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []interface{}:
		if value == nil {
			return value, nil
		}
		out := make([]interface{}, len(value))
		for index, child := range value {
			converted, err := registryMetadataForBSON(child)
			if err != nil {
				return nil, err
			}
			out[index] = converted
		}
		return out, nil
	case bson.M:
		converted, err := registryMetadataForBSON(map[string]interface{}(value))
		if err != nil {
			return nil, err
		}
		return bson.M(converted.(map[string]interface{})), nil
	case bson.A:
		converted, err := registryMetadataForBSON([]interface{}(value))
		if err != nil {
			return nil, err
		}
		return bson.A(converted.([]interface{})), nil
	default:
		return value, nil
	}
}
