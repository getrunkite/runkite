package natstransport

import "errors"

// A run ID is a literal partition key, never a NATS subject expression. Keep
// the existing JetStream KV alphabet and reject empty subject segments too.
// Encoding into a new namespace would require a separate queued-job and event
// history migration; this guard deliberately leaves valid existing keys intact.
var errInvalidRunID = errors.New("natstransport: run ID must use ASCII letters, digits, '-', '_', '/', '=' or single internal dots")

// ValidateRunID exposes the queue's existing literal-key boundary to callers
// before they create a durable run or take a synchronous cache fast path.
func (*Queue) ValidateRunID(id string) error { return validateRunID(id) }

func validateRunID(id string) error {
	if id == "" || id[0] == '.' || id[len(id)-1] == '.' {
		return errInvalidRunID
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '.' && i > 0 && id[i-1] == '.' {
			return errInvalidRunID
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '/' || c == '=' || c == '.' {
			continue
		}
		return errInvalidRunID
	}
	return nil
}
