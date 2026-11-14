// This external MCP server is a credential-aware consumer, not part of the
// control-plane binary. It runs one fixed read-only query using HTTP Basic
// credentials supplied privately by Runkite after managed authorization.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

type databaseExecutor struct{ config *pgx.ConnConfig }

func newExecutor(dsn string) (http.Handler, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Password != "" {
		return nil, fmt.Errorf("configure a PostgreSQL target without embedded credentials")
	}
	ip := net.ParseIP(cfg.Host)
	if cfg.Host != "localhost" && (ip == nil || !ip.IsLoopback()) && (cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify) {
		return nil, fmt.Errorf("non-loopback PostgreSQL requires verified TLS")
	}
	cfg.RuntimeParams = map[string]string{"application_name": "runkite_credential_executor", "default_transaction_read_only": "on", "statement_timeout": "5000"}
	cfg.ConnectTimeout = 5 * time.Second
	return &databaseExecutor{config: cfg}, nil
}

func (s *databaseExecutor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.JSONRPC != "2.0" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	respond := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	fail := func(code int, message string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": message}})
	}
	switch req.Method {
	case "initialize":
		respond(map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "runkite-vault-database-example", "version": "0.1.0"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		respond(map[string]any{})
	case "tools/list":
		respond(map[string]any{"tools": []any{map[string]any{"name": "report_count", "description": "Count synthetic reporting rows with a short-lived read-only database identity.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}})
	case "tools/call":
		if req.Params.Name != "report_count" || len(req.Params.Arguments) != 0 {
			fail(-32602, "only report_count with empty arguments is supported")
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user == "" || password == "" {
			fail(-32000, "database authorization unavailable")
			return
		}
		cfg := s.config.Copy()
		cfg.User = user
		cfg.Password = password
		conn, err := pgx.ConnectConfig(r.Context(), cfg)
		if err != nil {
			fail(-32000, "database authorization unavailable")
			return
		}
		defer conn.Close(r.Context())
		var count int64
		if err := conn.QueryRow(r.Context(), `SELECT count(*) FROM public.runkite_reports`).Scan(&count); err != nil {
			fail(-32000, "read-only report query failed")
			return
		}
		respond(map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("report_count=%d", count)}}})
	default:
		fail(-32601, "method not supported")
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8788", "MCP listen address")
	dsn := flag.String("postgres", "", "PostgreSQL DSN without a username/password")
	cert := flag.String("tls-cert", "", "TLS certificate path")
	key := flag.String("tls-key", "", "TLS private-key path")
	flag.Parse()
	handler, err := newExecutor(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	host, _, err := net.SplitHostPort(*listen)
	ip := net.ParseIP(host)
	if err != nil || ((host != "localhost" && (ip == nil || !ip.IsLoopback())) && (*cert == "" || *key == "")) {
		fmt.Fprintln(os.Stderr, "non-loopback MCP requires TLS")
		os.Exit(1)
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	if *cert != "" && *key != "" {
		err = server.ListenAndServeTLS(*cert, *key)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "MCP executor unavailable")
		os.Exit(1)
	}
}
