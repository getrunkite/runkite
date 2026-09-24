package config

import (
	"fmt"
	"time"

	"github.com/getrunkite/runkite/internal/payloadshrink"
)

const (
	payloadShrinkDefaultMaxBytes      = 32768
	payloadShrinkDefaultPreviewBytes  = 2048
	payloadShrinkDefaultMaxStoreBytes = 64 << 20
	payloadShrinkDefaultTTL           = 15 * time.Minute
	payloadShrinkMinTTL               = time.Minute
	payloadShrinkMaxTTL               = 24 * time.Hour
	payloadShrinkMinMaxBytes          = 1024
)

// PayloadShrinkEntry is the "payload_shrink" section of langgraph.json.
// Pointer fields distinguish omit (use default) from an explicit 0
// (preview_bytes 0 is valid: stub notice only).
type PayloadShrinkEntry struct {
	Enabled       bool   `json:"enabled"`
	MaxBytes      *int   `json:"max_bytes,omitempty"`
	PreviewBytes  *int   `json:"preview_bytes,omitempty"`
	CacheTTL      string `json:"cache_ttl,omitempty"`
	MaxStoreBytes *int   `json:"max_store_bytes,omitempty"`
}

// ParsePayloadShrink turns a first-file payload_shrink section into
// runtime settings. Nil entry is disabled with defaults filled (so
// retrieve still has numbers if a later toggle is added). Invalid
// numbers fail; cmd/serve exits on that error.
func ParsePayloadShrink(e *PayloadShrinkEntry) (payloadshrink.Settings, error) {
	s := payloadshrink.Settings{
		MaxBytes:      payloadShrinkDefaultMaxBytes,
		PreviewBytes:  payloadShrinkDefaultPreviewBytes,
		CacheTTL:      payloadShrinkDefaultTTL,
		MaxStoreBytes: payloadShrinkDefaultMaxStoreBytes,
	}
	if e == nil {
		return s, nil
	}
	s.Enabled = e.Enabled
	if e.MaxBytes != nil {
		s.MaxBytes = *e.MaxBytes
	}
	if e.PreviewBytes != nil {
		s.PreviewBytes = *e.PreviewBytes
	}
	if e.MaxStoreBytes != nil {
		s.MaxStoreBytes = *e.MaxStoreBytes
	}
	if e.CacheTTL != "" {
		d, err := time.ParseDuration(e.CacheTTL)
		if err != nil {
			return payloadshrink.Settings{}, fmt.Errorf("payload_shrink.cache_ttl: %w", err)
		}
		s.CacheTTL = d
	}
	if err := s.Validate(); err != nil {
		return payloadshrink.Settings{}, err
	}
	return s, nil
}
