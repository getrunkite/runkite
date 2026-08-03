// Package pagecursor encodes opaque keyset pagination tokens for Admin
// list endpoints. Agent Protocol client search stays limit/offset-only.
package pagecursor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// HeaderNextCursor is set on Admin list responses when another page exists
// (len(items) == limit). Clients pass the value back as ?cursor=.
const HeaderNextCursor = "X-Next-Cursor"

// TimeCursor resumes a created_at DESC, id DESC listing.
type TimeCursor struct {
	Time time.Time
	ID   string
}

// KeyCursor resumes a name ASC, id ASC listing (agents: agent_id;
// registry: tenant_id as the secondary key under system context).
type KeyCursor struct {
	Key string
	ID  string
}

type wire struct {
	T string `json:"t,omitempty"`
	K string `json:"k,omitempty"`
	I string `json:"i"`
}

// EncodeTime builds an opaque cursor from the last row of a time-ordered page.
func EncodeTime(t time.Time, id string) string {
	return encode(wire{T: t.UTC().Format(time.RFC3339Nano), I: id})
}

// DecodeTime parses a time-ordered cursor. Empty input returns (zero, nil).
func DecodeTime(s string) (TimeCursor, error) {
	if s == "" {
		return TimeCursor{}, nil
	}
	w, err := decode(s)
	if err != nil {
		return TimeCursor{}, err
	}
	if w.T == "" || w.I == "" || w.K != "" {
		return TimeCursor{}, fmt.Errorf("invalid time cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, w.T)
	if err != nil {
		return TimeCursor{}, fmt.Errorf("invalid time cursor: %w", err)
	}
	return TimeCursor{Time: ts.UTC(), ID: w.I}, nil
}

// EncodeKey builds an opaque cursor from the last row of a name-ordered page.
func EncodeKey(key, id string) string {
	return encode(wire{K: key, I: id})
}

// DecodeKey parses a name-ordered cursor. Empty input returns (zero, nil).
func DecodeKey(s string) (KeyCursor, error) {
	if s == "" {
		return KeyCursor{}, nil
	}
	w, err := decode(s)
	if err != nil {
		return KeyCursor{}, err
	}
	if w.K == "" || w.I == "" || w.T != "" {
		return KeyCursor{}, fmt.Errorf("invalid key cursor")
	}
	return KeyCursor{Key: w.K, ID: w.I}, nil
}

func encode(w wire) string {
	b, _ := json.Marshal(w)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decode(s string) (wire, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return wire{}, fmt.Errorf("invalid cursor encoding")
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return wire{}, fmt.Errorf("invalid cursor payload")
	}
	return w, nil
}
