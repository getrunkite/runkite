package payloadshrink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// Live is true when shrink may rewrite a tools/call body.
func Live(s Settings, store Store) bool {
	return s.Enabled && store != nil
}

// Apply rewrites a successful JSON-RPC tools/call body when it is over
// max_bytes and the stash fits the run+generation budget. On any store
// error it returns the original body. Downstream already succeeded, so
// this never fails the MCP call.
func Apply(ctx context.Context, store Store, s Settings, runID string, generation int64, respBody []byte) []byte {
	if !Live(s, store) || runID == "" {
		return respBody
	}
	if len(respBody) <= s.MaxBytes {
		return respBody
	}
	env, inner, ok := innerResult(respBody)
	if !ok {
		return respBody
	}
	baseRef := contentAddressRef(inner)
	vkey, ok := pickValueKey(ctx, store, runID, generation, baseRef, inner)
	if !ok {
		return respBody
	}
	useRef := refFromKey(vkey, runID, generation)
	stub, err := buildStub(env, previewText(inner, s.PreviewBytes), useRef)
	if err != nil || len(stub) >= len(respBody) {
		return respBody
	}

	existing, err := store.Get(ctx, vkey)
	if err == nil && bytes.Equal(existing, inner) {
		return stub
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return respBody
	}

	n := int64(len(inner))
	bkey := budgetKey(runID, generation)
	total, err := store.IncrBy(ctx, bkey, n)
	if err != nil {
		return respBody
	}
	_ = store.Expire(ctx, bkey, s.CacheTTL)
	// ponytail: INCRBY then check is a brake, not a hard quota. Two
	// concurrent stashes can overshoot max_store_bytes by one payload.
	// Upgrade: Lua INCRBY+compare+maybe DECRBY if we ever need a fence.
	if total > int64(s.MaxStoreBytes) {
		_, _ = store.DecrBy(ctx, bkey, n)
		return respBody
	}
	if err := store.Set(ctx, vkey, inner, s.CacheTTL); err != nil {
		_, _ = store.DecrBy(ctx, bkey, n)
		return respBody
	}
	return stub
}

func pickValueKey(ctx context.Context, store Store, runID string, generation int64, ref string, inner []byte) (string, bool) {
	for _, r := range []string{ref, ref + "-2"} {
		k := valueKey(runID, generation, r)
		got, err := store.Get(ctx, k)
		if errors.Is(err, ErrNotFound) {
			return k, true
		}
		if err != nil {
			return "", false
		}
		if bytes.Equal(got, inner) {
			return k, true
		}
	}
	return "", false
}

func refFromKey(key, runID string, generation int64) string {
	prefix := valueKey(runID, generation, "")
	if len(key) <= len(prefix) {
		return ""
	}
	return key[len(prefix):]
}

// Retrieve returns the stashed inner MCP result for this run+generation.
func Retrieve(ctx context.Context, store Store, s Settings, runID string, generation int64, ref string) ([]byte, error) {
	if !Live(s, store) {
		return nil, errDisabled
	}
	if ref == "" {
		return nil, errMissingRef
	}
	if runID == "" {
		return nil, ErrNotFound
	}
	got, err := store.Get(ctx, valueKey(runID, generation, ref))
	if err != nil {
		return nil, ErrNotFound
	}
	return got, nil
}

var (
	errDisabled   = errors.New("payload_shrink_disabled")
	errMissingRef = errors.New("missing ref")
)

func IsDisabled(err error) bool   { return errors.Is(err, errDisabled) }
func IsMissingRef(err error) bool { return errors.Is(err, errMissingRef) }

type retrieveArgs struct {
	Ref string `json:"ref"`
}

func ParseRef(args json.RawMessage) string {
	var a retrieveArgs
	_ = json.Unmarshal(args, &a)
	return a.Ref
}
