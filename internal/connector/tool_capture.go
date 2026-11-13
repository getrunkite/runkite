package connector

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/getrunkite/runkite/internal/jsonvalue"
)

// rawToolCapture is request-local protocol evidence, not a cache or export.
// The SDK's typed Tool has interface-valued schemas decoded through float64.
// Capture the complete advertised definition before that lossy conversion.
type rawToolCapture struct {
	mu    sync.Mutex
	pages map[string]*rawToolPage
	bytes int64
	err   error
}
type rawToolCaptureKey struct{}
type rawToolPage struct {
	result json.RawMessage
	tools  []map[string]any
	next   string
}

func newRawToolCapture() *rawToolCapture { return &rawToolCapture{pages: map[string]*rawToolPage{}} }
func (c *rawToolCapture) failure() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *rawToolCapture) fail()          { c.mu.Lock(); defer c.mu.Unlock(); c.err = ErrToolRevisionUnavailable }
func (c *rawToolCapture) page(cursor string) (*rawToolPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pages[cursor]
	if c.err != nil || p == nil {
		return nil, ErrToolRevisionUnavailable
	}
	return p, nil
}

func protocolKeyCase(object map[string]any, names ...string) bool {
	for key := range object {
		for _, canonical := range names {
			if strings.EqualFold(key, canonical) && key != canonical {
				return false
			}
		}
	}
	return true
}

func parseRawToolPage(raw []byte, requestID json.RawMessage) (*rawToolPage, bool, error) {
	value, err := jsonvalue.DecodeUnique(raw)
	if err != nil {
		return nil, false, ErrToolRevisionUnavailable
	}
	envelope, ok := value.(map[string]any)
	if !ok || !protocolKeyCase(envelope, "jsonrpc", "id", "method", "params", "result", "error") || envelope["jsonrpc"] != "2.0" {
		return nil, false, ErrToolRevisionUnavailable
	}
	if _, notification := envelope["method"]; notification {
		return nil, false, nil
	}
	wanted, err := jsonvalue.DecodeUnique(requestID)
	if err != nil {
		return nil, false, ErrToolRevisionUnavailable
	}
	id, present := envelope["id"]
	if !present || id == nil || !jsonvalue.Equal(id, wanted) {
		return nil, false, nil
	}
	result, ok := envelope["result"].(map[string]any)
	_, hasError := envelope["error"]
	if !ok || hasError || !protocolKeyCase(result, "tools", "nextCursor", "_meta") {
		return nil, true, ErrToolRevisionUnavailable
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) > 4096 {
		return nil, true, ErrToolRevisionUnavailable
	}
	page := &rawToolPage{}
	seen := map[string]bool{}
	for _, item := range tools {
		definition, ok := item.(map[string]any)
		if !ok || !protocolKeyCase(definition, "name", "title", "description", "inputSchema", "outputSchema", "annotations", "icons", "_meta") {
			return nil, true, ErrToolRevisionUnavailable
		}
		name, ok := definition["name"].(string)
		if !ok || name == "" || len(name) > 1024 || seen[name] {
			return nil, true, ErrToolRevisionUnavailable
		}
		seen[name] = true
		if annotations, ok := definition["annotations"].(map[string]any); ok && !protocolKeyCase(annotations, "title", "readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint") {
			return nil, true, ErrToolRevisionUnavailable
		}
		page.tools = append(page.tools, definition)
	}
	if next, exists := result["nextCursor"]; exists && next != nil {
		var ok bool
		page.next, ok = next.(string)
		if !ok || len(page.next) > 4096 {
			return nil, true, ErrToolRevisionUnavailable
		}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, true, ErrToolRevisionUnavailable
	}
	page.result = append(json.RawMessage(nil), fields["result"]...)
	return page, true, nil
}

// rawToolListRequest identifies a single SDK request and its exact ID/cursor.
// It never reads the live Body, so it does not consume the SDK's request.
func rawToolListRequest(r *http.Request) (json.RawMessage, string, bool, error) {
	if r.Method != http.MethodPost || r.GetBody == nil {
		return nil, "", false, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, "", false, ErrToolRevisionUnavailable
	}
	defer body.Close()
	raw, err := readLimitedBody(body, MaxMCPRequestBytes)
	if err != nil {
		return nil, "", false, ErrToolRevisionUnavailable
	}
	var request struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return nil, "", false, ErrToolRevisionUnavailable
	}
	if request.Method != "tools/list" {
		return nil, "", false, nil
	}
	if _, err := jsonvalue.DecodeUnique(raw); err != nil || len(request.ID) == 0 || len(request.Params.Cursor) > 4096 {
		return nil, "", true, ErrToolRevisionUnavailable
	}
	return request.ID, request.Params.Cursor, true, nil
}

func (c *rawToolCapture) capture(r *http.Request, response *http.Response) (bool, error) {
	id, cursor, listing, err := rawToolListRequest(r)
	if err != nil {
		c.fail()
		return listing, ErrToolRevisionUnavailable
	}
	if !listing {
		return false, nil
	}
	c.mu.Lock()
	remaining := MaxMCPResponseBytes - c.bytes
	duplicate := c.pages[cursor] != nil
	c.mu.Unlock()
	if duplicate || remaining <= 0 || response.StatusCode != http.StatusOK {
		c.fail()
		return true, ErrToolRevisionUnavailable
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		c.fail()
		return true, ErrToolRevisionUnavailable
	}
	var wire []byte
	var page *rawToolPage
	switch media {
	case "application/json":
		wire, err = readLimitedBody(response.Body, remaining)
		if err == nil {
			var matched bool
			page, matched, err = parseRawToolPage(wire, id)
			if !matched && err == nil {
				err = ErrToolRevisionUnavailable
			}
		}
	case "text/event-stream":
		wire, page, err = captureToolSSE(response.Body, id, remaining)
	default:
		err = ErrToolRevisionUnavailable
	}
	if err != nil || page == nil {
		c.fail()
		return true, ErrToolRevisionUnavailable
	}
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(wire))
	response.ContentLength = int64(len(wire))
	response.Header.Del("Content-Length")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pages[cursor] != nil || c.bytes+int64(len(wire)) > MaxMCPResponseBytes {
		c.err = ErrToolRevisionUnavailable
		return true, ErrToolRevisionUnavailable
	}
	c.pages[cursor] = page
	c.bytes += int64(len(wire))
	return true, nil
}

// Stop at the correlated response, rather than waiting for a server to close a
// POST SSE stream. Replay that bounded prefix to the SDK for normal processing.
func captureToolSSE(body io.Reader, id json.RawMessage, limit int64) ([]byte, *rawToolPage, error) {
	reader := bufio.NewReader(io.LimitReader(body, limit+1))
	var wire, line, data bytes.Buffer
	event := ""
	firstLine := true
	for {
		line.Reset()
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return nil, nil, ErrToolRevisionUnavailable
			}
			wire.WriteByte(b)
			if int64(wire.Len()) > limit {
				return nil, nil, ErrToolRevisionUnavailable
			}
			if b == '\r' || b == '\n' {
				if b == '\r' {
					if next, err := reader.Peek(1); err == nil && next[0] == '\n' {
						b, _ = reader.ReadByte()
						wire.WriteByte(b)
						if int64(wire.Len()) > limit {
							return nil, nil, ErrToolRevisionUnavailable
						}
					}
				}
				break
			}
			line.WriteByte(b)
		}
		text := line.String()
		if firstLine {
			text = strings.TrimPrefix(text, "\ufeff")
			firstLine = false
		}
		if text == "" {
			if data.Len() > 0 && (event == "" || event == "message") {
				payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
				page, matched, err := parseRawToolPage(payload, id)
				if err != nil {
					return nil, nil, err
				}
				if matched {
					return wire.Bytes(), page, nil
				}
			}
			data.Reset()
			event = ""
			continue
		}
		if strings.HasPrefix(text, ":") {
			continue
		}
		field, value, _ := strings.Cut(text, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "event":
			event = value
		}
	}
}
