package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"
)

// httpClient is shared by OAuth token fetches and the MCP proxy forwarder.
// http.DefaultClient has no timeout and can hang a connector request (and
// the caller waiting on it) forever if the IdP or downstream MCP server
// stalls.
var httpClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Transport and body-reader errors may contain endpoint credentials or response
// content. Keep only fixed classifications used for cancellation, bounded reads
// and connector error mappings. Never wrap the original error.
func safeConnectorRequestError(ctx context.Context, operation string, err error) error {
	var causes []error
	for _, cause := range []error{
		context.Canceled, context.DeadlineExceeded, ErrBodyTooLarge, io.ErrUnexpectedEOF, io.EOF,
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH, syscall.EHOSTUNREACH,
		syscall.ETIMEDOUT, syscall.EPIPE,
	} {
		if errors.Is(err, cause) || ctx != nil && errors.Is(ctx.Err(), cause) {
			causes = append(causes, cause)
		}
	}
	if len(causes) > 0 {
		return fmt.Errorf("%s: %w", operation, errors.Join(causes...))
	}
	return errors.New(operation)
}
