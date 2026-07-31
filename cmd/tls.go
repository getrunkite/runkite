package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// serverTLSConfig builds a *tls.Config for either the HTTP or gRPC
// server from a cert/key pair and an optional client CA. Both surfaces
// share this one loader because the shape of the decision is identical
// for each: certFile+keyFile enable TLS at all (server authentication);
// clientCAFile additionally enables mTLS (client authentication) by
// requiring and verifying a client certificate signed by that CA.
// Deliberately env-var-driven, not langgraph.json -- matches every
// other piece of deployment infrastructure this project already
// configures this way (POSTGRES_DSN, REDIS_URL, RUNNER_TOKEN_*), not
// business/agent config.
//
// Returns (nil, nil) if certFile/keyFile are both empty -- TLS is
// opt-in, off by default, same convention as every other platform
// extension in this codebase.
func serverTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("both cert and key file must be set together (got cert=%q key=%q)", certFile, keyFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key pair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAFile == "" {
		return cfg, nil
	}
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in client CA file %q", clientCAFile)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}
