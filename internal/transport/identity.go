package transport

// RunIDValidator lets a transport reject identifiers its literal partition/key
// format cannot represent. Run creation checks this optional boundary before
// creating records, including synchronous cache hits that never enqueue work.
// Other transports retain their existing identifier support.
type RunIDValidator interface {
	ValidateRunID(string) error
}
