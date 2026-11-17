package transport

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// RunRecordKey gives every record, including an explicitly legacy empty
// identity, its own backend-safe namespace. Length framing preserves exact
// opaque bytes without delimiter or JSON Unicode normalization aliases.
func RunRecordKey(runID, incarnation string) string {
	frame := binary.BigEndian.AppendUint64(nil, uint64(len(runID)))
	frame = append(frame, runID...)
	frame = append(frame, incarnation...)
	sum := sha256.Sum256(frame)
	return "ri1_" + hex.EncodeToString(sum[:])
}

// CheckRunEventIdentity prevents a stored event from claiming another record.
func CheckRunEventIdentity(event *RunEvent, incarnation string) error {
	if event == nil || event.RunIncarnationID != incarnation {
		return errors.New("event run identity mismatch")
	}
	return nil
}
