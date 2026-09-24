package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/payloadshrink"
)

func (s *Server) SetPayloadShrink(cfg payloadshrink.Settings, store payloadshrink.Store) {
	s.payloadShrink = cfg
	s.payloadStore = store
}

func (s *Server) payloadShrinkLive() bool {
	return payloadshrink.Live(s.payloadShrink, s.payloadStore)
}

func (s *Server) servePayloadRetrieve(w http.ResponseWriter, r *http.Request, rpcID json.RawMessage, args json.RawMessage) {
	ref := payloadshrink.ParseRef(args)
	binding := auth.RunBindingFromContext(r.Context())
	runID := ""
	var gen int64
	if binding != nil {
		runID = binding.RunID
		gen = binding.Generation
	}
	inner, err := payloadshrink.Retrieve(r.Context(), s.payloadStore, s.payloadShrink, runID, gen, ref)
	if payloadshrink.IsMissingRef(err) {
		s.writePayloadRPCError(w, rpcID, "missing ref")
		return
	}
	if payloadshrink.IsDisabled(err) {
		s.writePayloadRPCError(w, rpcID, "payload_shrink_disabled")
		return
	}
	if err != nil {
		s.writePayloadRPCError(w, rpcID, "payload_not_found")
		return
	}
	out, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: "2.0", ID: rpcID, Result: inner})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build retrieve response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Server) writePayloadRPCError(w http.ResponseWriter, rpcID json.RawMessage, message string) {
	denied, err := connector.DeniedRPCResult(rpcID, message, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build retrieve response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(denied.StatusCode)
	_, _ = w.Write(denied.Body)
}

func (s *Server) maybeShrinkMCP(ctx context.Context, status int, body []byte) []byte {
	if status != http.StatusOK || !s.payloadShrinkLive() {
		return body
	}
	binding := auth.RunBindingFromContext(ctx)
	if binding == nil {
		return body
	}
	return payloadshrink.Apply(ctx, s.payloadStore, s.payloadShrink, binding.RunID, binding.Generation, body)
}

func (s *Server) maybeInjectRetrieveTool(method string, body []byte) []byte {
	if method != "tools/list" || !s.payloadShrinkLive() {
		return body
	}
	return payloadshrink.InjectRetrieveTool(body)
}
