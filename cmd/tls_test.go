package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genSelfSignedCert writes a minimal self-signed ECDSA cert/key pair
// (PEM) to dir, returning their paths. Good enough to exercise
// tls.LoadX509KeyPair and x509.CertPool parsing without pulling in a
// real CA or shelling out to openssl.
func genSelfSignedCert(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, name+"-cert.pem")
	keyPath = filepath.Join(dir, name+"-key.pem")
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatal(err)
	}
	keyOut.Close()
	return certPath, keyPath
}

func TestServerTLSConfig_BothEmptyReturnsNilNil(t *testing.T) {
	cfg, err := serverTLSConfig("", "", "")
	if err != nil || cfg != nil {
		t.Fatalf("expected (nil, nil) when TLS is unconfigured, got (%v, %v)", cfg, err)
	}
}

func TestServerTLSConfig_OnlyCertSetIsAnError(t *testing.T) {
	if _, err := serverTLSConfig("cert.pem", "", ""); err == nil {
		t.Fatal("expected an error when only certFile is set without keyFile")
	}
	if _, err := serverTLSConfig("", "key.pem", ""); err == nil {
		t.Fatal("expected an error when only keyFile is set without certFile")
	}
}

func TestServerTLSConfig_LoadsCertAndEnablesTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genSelfSignedCert(t, dir, "server")

	cfg, err := serverTLSConfig(certPath, keyPath, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected a non-nil TLS config")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected 1 loaded certificate, got %d", len(cfg.Certificates))
	}
	if cfg.ClientCAs != nil {
		t.Error("expected ClientCAs to be nil when no client CA file is given (mTLS not requested)")
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Error("expected ClientAuth to stay at its zero value (no client cert required) without a client CA")
	}
}

func TestServerTLSConfig_ClientCAEnablesMTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genSelfSignedCert(t, dir, "server")
	caCertPath, _ := genSelfSignedCert(t, dir, "client-ca")

	cfg, err := serverTLSConfig(certPath, keyPath, caCertPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("expected ClientCAs to be populated when a client CA file is given")
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected ClientAuth to require+verify a client cert (mTLS), got %v", cfg.ClientAuth)
	}
}

func TestServerTLSConfig_MissingCertFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if _, err := serverTLSConfig(filepath.Join(dir, "nonexistent-cert.pem"), filepath.Join(dir, "nonexistent-key.pem"), ""); err == nil {
		t.Fatal("expected an error when the cert/key files don't exist")
	}
}

func TestServerTLSConfig_InvalidClientCAFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genSelfSignedCert(t, dir, "server")
	badCA := filepath.Join(dir, "not-a-cert.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serverTLSConfig(certPath, keyPath, badCA); err == nil {
		t.Fatal("expected an error when the client CA file has no valid PEM certificates")
	}
}
