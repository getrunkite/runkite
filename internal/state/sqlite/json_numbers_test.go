package sqlite_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/conformance"
	"github.com/getrunkite/runkite/internal/state/sqlite"
)

func TestSQLiteExactJSONNumbers(t *testing.T) {
	conformance.RunExactJSONNumberTests(t, func(t *testing.T) state.Store {
		s, err := sqlite.New("")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestSQLiteExactNumericStoreFilters(t *testing.T) {
	s, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	for key, number := range map[string]string{
		"signed_even": "9007199254740992", "signed_odd": "9007199254740993",
		"unsigned_even": "18446744073709551614", "unsigned_odd": "18446744073709551615",
		"short_decimal": "0.1", "long_decimal": "0.1000000000000000000000001",
		"exponent": "1e30", "negative": "-0.1", "huge_exponent": "1e9999999999999999999",
	} {
		if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"numeric"}, Key: key, Value: map[string]interface{}{"number": json.Number(number)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"numeric"}, Key: "text", Value: map[string]interface{}{"number": "9007199254740993"}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ filter, key string }{
		{"9007199254740993", "signed_odd"}, {"9007199254740992", "signed_even"},
		{"18446744073709551615", "unsigned_odd"}, {"18446744073709551614", "unsigned_even"},
		{"0.1", "short_decimal"}, {"0.1000000000000000000000001", "long_decimal"},
		{"1000000000000000000000000000000", "exponent"}, {"-1e-1", "negative"},
		{"10e9999999999999999998", "huge_exponent"},
	} {
		t.Run(test.key, func(t *testing.T) {
			items, err := s.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"numeric"}, Filter: map[string]interface{}{"number": json.Number(test.filter)}, Limit: 10})
			if err != nil || len(items) != 1 || items[0].Key != test.key {
				t.Fatalf("filter %s: items=%v err=%v", test.filter, items, err)
			}
		})
	}
	for _, bad := range []interface{}{json.Number("1e+"), math.NaN()} {
		if _, err := s.SearchItems(ctx, &models.StoreSearchRequest{Filter: map[string]interface{}{"number": bad}}); err == nil {
			t.Fatalf("invalid numeric filter accepted: %v", bad)
		}
	}
}
