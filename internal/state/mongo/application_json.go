package mongo

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Application JSON uses native BSON numbers without passing json.Number through
// the driver's string codec. Int64 and exact Decimal128 are the supported range;
// unsupported precision fails before a document is written.
func applicationToBSON(value interface{}) (interface{}, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode Mongo application JSON: %w", err)
	}
	return applicationJSONToBSON(raw)
}

func applicationJSONToBSON(raw json.RawMessage) (interface{}, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value interface{}
	if err := jsonvalue.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode Mongo application JSON: %w", err)
	}
	return applicationNumberToBSON(value)
}

func applicationNumberToBSON(value interface{}) (interface{}, error) {
	switch value := value.(type) {
	case json.Number:
		canonical, err := jsonvalue.CanonicalNumber(value)
		if err != nil {
			return nil, err
		}
		// Canonicalization removes large runs of trailing coefficient zeroes
		// before the Decimal128 driver can normalize them with big.Int division.
		// Preserve native int64 for ordinary integral spellings when possible.
		if integer, err := value.Int64(); err == nil {
			return integer, nil
		}
		if integer, err := canonical.Int64(); err == nil {
			return integer, nil
		}
		coefficient := canonical.String()
		if index := strings.IndexByte(coefficient, 'e'); index >= 0 {
			coefficient = coefficient[:index]
		}
		if len(strings.TrimPrefix(coefficient, "-")) > 34 {
			return nil, fmt.Errorf("mongo application number exceeds exact int64/Decimal128 range")
		}
		decimal, err := bson.ParseDecimal128(canonical.String())
		if err != nil {
			return nil, fmt.Errorf("mongo application number exceeds exact int64/Decimal128 range")
		}
		return decimal, nil
	case map[string]interface{}:
		out := make(map[string]interface{}, len(value))
		for key, item := range value {
			converted, err := applicationNumberToBSON(item)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(value))
		for index, item := range value {
			converted, err := applicationNumberToBSON(item)
			if err != nil {
				return nil, err
			}
			out[index] = converted
		}
		return out, nil
	default:
		return value, nil
	}
}

// Decimal128 equality does not always match historical BSON double values.
// Add the old double representation only when its JSON value is unchanged;
// otherwise a rounded neighbouring integer could match a different value.
func applicationEqualityFilter(value interface{}) (interface{}, error) {
	native, err := applicationToBSON(value)
	if err != nil {
		return nil, err
	}
	if _, decimal := native.(bson.Decimal128); !decimal {
		return native, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded interface{}
	if err := jsonvalue.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	number, numeric := decoded.(json.Number)
	if !numeric {
		return native, nil
	}
	legacy, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(legacy, 0) || math.IsNaN(legacy) {
		return native, nil
	}
	roundTrip := json.Number(strconv.FormatFloat(legacy, 'g', -1, 64))
	if !jsonvalue.EqualNumbers(number, roundTrip) {
		return native, nil
	}
	return bson.M{"$in": bson.A{native, legacy}}, nil
}

func normalizeApplicationBSON(value interface{}) (interface{}, error) {
	switch value := value.(type) {
	case bson.M:
		return normalizeApplicationBSON(map[string]interface{}(value))
	case map[string]interface{}:
		out := make(map[string]interface{}, len(value))
		for key, item := range value {
			converted, err := normalizeApplicationBSON(item)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case bson.A:
		return normalizeApplicationBSON([]interface{}(value))
	case []interface{}:
		out := make([]interface{}, len(value))
		for index, item := range value {
			converted, err := normalizeApplicationBSON(item)
			if err != nil {
				return nil, err
			}
			out[index] = converted
		}
		return out, nil
	case int32:
		return json.Number(strconv.FormatInt(int64(value), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(value, 10)), nil
	case bson.Decimal128:
		raw := value.String()
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("mongo application number is not finite JSON")
		}
		return json.Number(raw), nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("mongo application number is not finite JSON")
		}
		return json.Number(strconv.FormatFloat(value, 'g', -1, 64)), nil
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("mongo application number is not finite JSON")
		}
		return json.Number(strconv.FormatFloat(float64(value), 'g', -1, 32)), nil
	default:
		return value, nil
	}
}

func applicationJSON(value interface{}) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	converted, err := normalizeApplicationBSON(value)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("encode Mongo application JSON: %w", err)
	}
	return raw, nil
}

func applicationMap(value interface{}) (map[string]interface{}, error) {
	raw, err := applicationJSON(value)
	if err != nil || raw == nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := jsonvalue.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode Mongo application object: %w", err)
	}
	return out, nil
}

func applicationSlice(value interface{}) ([]interface{}, error) {
	raw, err := applicationJSON(value)
	if err != nil || raw == nil {
		return nil, err
	}
	var out []interface{}
	if err := jsonvalue.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode Mongo application array: %w", err)
	}
	return out, nil
}
