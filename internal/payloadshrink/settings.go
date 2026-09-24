package payloadshrink

import (
	"fmt"
	"time"
)

// ToolName is injected on tools/list and intercepted on tools/call so
// the runner's MCP client never forwards the retrieve to the connector.
const ToolName = "runkite_retrieve_payload"

const (
	valueKeyPrefix  = "rk:payload:v:"
	budgetKeyPrefix = "rk:payload:b:"
)

// Settings is the runtime shrink config after defaults and validation.
type Settings struct {
	Enabled       bool
	MaxBytes      int
	PreviewBytes  int
	CacheTTL      time.Duration
	MaxStoreBytes int
}

// Validate enforces the fail-loud bounds used at serve startup.
func (s Settings) Validate() error {
	if s.MaxBytes < 1024 {
		return fmt.Errorf("payload_shrink.max_bytes must be >= 1024, got %d", s.MaxBytes)
	}
	if s.PreviewBytes < 0 {
		return fmt.Errorf("payload_shrink.preview_bytes must be >= 0, got %d", s.PreviewBytes)
	}
	if s.PreviewBytes >= s.MaxBytes {
		return fmt.Errorf("payload_shrink.preview_bytes must be < max_bytes")
	}
	if s.MaxStoreBytes < s.MaxBytes {
		return fmt.Errorf("payload_shrink.max_store_bytes must be >= max_bytes")
	}
	if s.CacheTTL < time.Minute || s.CacheTTL > 24*time.Hour {
		return fmt.Errorf("payload_shrink.cache_ttl must be in [1m, 24h], got %s", s.CacheTTL)
	}
	return nil
}

func valueKey(runID string, generation int64, ref string) string {
	return fmt.Sprintf("%s%s:%d:%s", valueKeyPrefix, runID, generation, ref)
}

func budgetKey(runID string, generation int64) string {
	return fmt.Sprintf("%s%s:%d", budgetKeyPrefix, runID, generation)
}
