package connector

import (
	"errors"
	"io"
)

const (
	MaxMCPRequestBytes    int64 = 1 << 20
	MaxMCPResponseBytes   int64 = 8 << 20
	MaxOAuthResponseBytes int64 = 1 << 20
)

// ErrBodyTooLarge is returned before oversized data is forwarded or decoded.
// A downstream tool may already have executed when its response exceeds a cap;
// callers must not retry a write tool automatically after this error.
var ErrBodyTooLarge = errors.New("connector body exceeds size limit")

func readLimitedBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrBodyTooLarge
	}
	return body, nil
}
