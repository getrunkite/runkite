package models

import "testing"

func TestRunIsSimulation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		meta map[string]interface{}
		want bool
	}{
		{"nil", nil, false},
		{"empty", map[string]interface{}{}, false},
		{"bool true", map[string]interface{}{"simulation": true}, true},
		{"bool false", map[string]interface{}{"simulation": false}, false},
		{"string true", map[string]interface{}{"simulation": "true"}, true},
		{"string 1", map[string]interface{}{"simulation": "1"}, true},
		{"string yes", map[string]interface{}{"simulation": "YES"}, true},
		{"string no", map[string]interface{}{"simulation": "no"}, false},
		{"other key", map[string]interface{}{"cache_hit": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunIsSimulation(tc.meta); got != tc.want {
				t.Fatalf("RunIsSimulation(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}
