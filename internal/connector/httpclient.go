package connector

import (
	"net/http"
	"time"
)

// httpClient is shared by OAuth token fetches and the MCP proxy forwarder.
// http.DefaultClient has no timeout and can hang a connector request (and
// the caller waiting on it) forever if the IdP or downstream MCP server
// stalls.
var httpClient = &http.Client{Timeout: 30 * time.Second}
