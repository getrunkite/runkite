package credentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type VaultConfig struct {
	Address               string `json:"address"`
	Namespace             string `json:"namespace,omitempty"`
	TokenFile             string `json:"-"`
	Mount                 string `json:"mount"`
	Role                  string `json:"role"`
	AllowInsecureLoopback bool   `json:"-"`
}

var pathPartPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type VaultProvider struct {
	cfg    VaultConfig
	client *http.Client
}
type providerError struct{ notSent bool }

func (*providerError) Error() string { return "Vault credential operation unavailable" }

func NewVaultProvider(cfg VaultConfig) (*VaultProvider, error) {
	u, err := url.Parse(cfg.Address)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, ErrUnavailable
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !cfg.AllowInsecureLoopback || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, ErrUnavailable
		}
	}
	if !validMount(cfg.Mount) || !pathPartPattern.MatchString(cfg.Role) || cfg.TokenFile == "" || !validNamespace(cfg.Namespace) {
		return nil, ErrUnavailable
	}
	cfg.Address = strings.TrimRight(cfg.Address, "/")
	// A database GET issues credentials and is not safely replayable. Disabling
	// connection reuse prevents net/http's replay of GET on a reused connection.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &VaultProvider{cfg: cfg, client: client}, nil
}

func validMount(mount string) bool {
	if mount == "" || len(mount) > 256 {
		return false
	}
	for _, part := range strings.Split(mount, "/") {
		if !pathPartPattern.MatchString(part) {
			return false
		}
	}
	return true
}
func validNamespace(ns string) bool { return ns == "" || validMount(strings.TrimSuffix(ns, "/")) }

// Revision identifies the configured provider and role, not resolved secrets.
func (p *VaultProvider) Revision(consumer string, maxTTL, increment time.Duration) string {
	b, _ := json.Marshal(struct {
		Address, Namespace, Mount, Role, Consumer string
		MaxTTL, Increment                         int64
	}{p.cfg.Address, p.cfg.Namespace, p.cfg.Mount, p.cfg.Role, consumer, int64(maxTTL), int64(increment)})
	sum := sha256.Sum256(b)
	return "vault-db-v1-" + hex.EncodeToString(sum[:])
}

func (p *VaultProvider) handleValid(handle string) bool {
	return strings.HasPrefix(handle, p.cfg.Mount+"/creds/"+p.cfg.Role+"/") && len(handle) > len(p.cfg.Mount)+len(p.cfg.Role)+8 && len(handle) <= 1024 && !strings.ContainsFunc(handle, unicode.IsControl)
}

func (p *VaultProvider) request(ctx context.Context, method, path string, payload any) ([]byte, int, error) {
	token, err := ReadPrivateFile(p.cfg.TokenFile, 16384)
	if err != nil {
		return nil, 0, &providerError{notSent: true}
	}
	defer clear(token)
	token = bytes.TrimSpace(token)
	if len(token) == 0 || bytes.ContainsAny(token, "\r\n\x00") {
		return nil, 0, &providerError{notSent: true}
	}
	var body []byte
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, &providerError{notSent: true}
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.Address+"/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, &providerError{notSent: true}
	}
	req.Header.Set("X-Vault-Token", string(token))
	if p.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", p.cfg.Namespace)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, &providerError{}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, resp.StatusCode, &providerError{}
	}
	return data, resp.StatusCode, nil
}

type vaultGrant struct {
	LeaseID       string `json:"lease_id"`
	LeaseDuration int64  `json:"lease_duration"`
	Renewable     bool   `json:"renewable"`
	Data          struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"data"`
}

func grantTTL(seconds int64) (time.Duration, bool) {
	if seconds <= 0 || seconds > 365*24*3600 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func (p *VaultProvider) Issue(ctx context.Context) (Grant, error) {
	b, status, err := p.request(ctx, http.MethodGet, p.cfg.Mount+"/creds/"+p.cfg.Role, nil)
	if err != nil {
		return Grant{}, err
	}
	defer clear(b)
	var v vaultGrant
	if status != http.StatusOK || json.Unmarshal(b, &v) != nil || !p.handleValid(v.LeaseID) {
		return Grant{}, &providerError{}
	}
	ttl, valid := grantTTL(v.LeaseDuration)
	g := Grant{Handle: v.LeaseID, TTL: ttl, Renewable: v.Renewable, Credential: UserPassword{v.Data.Username, v.Data.Password}}
	if !valid || v.Data.Username == "" || v.Data.Password == "" || len(v.Data.Username) > 4096 || len(v.Data.Password) > 16384 || strings.ContainsRune(v.Data.Username, 0) || strings.ContainsRune(v.Data.Password, 0) {
		return g, &providerError{}
	}
	return g, nil
}

func (p *VaultProvider) Renew(ctx context.Context, handle string, increment time.Duration) (Grant, error) {
	if !p.handleValid(handle) || increment <= 0 {
		return Grant{}, &providerError{notSent: true}
	}
	b, status, err := p.request(ctx, http.MethodPost, "sys/leases/renew", map[string]any{"lease_id": handle, "increment": int64(increment / time.Second)})
	if err != nil {
		return Grant{}, err
	}
	defer clear(b)
	var v vaultGrant
	if status != http.StatusOK || json.Unmarshal(b, &v) != nil || v.LeaseID != handle {
		return Grant{}, &providerError{}
	}
	ttl, valid := grantTTL(v.LeaseDuration)
	if !valid {
		return Grant{}, &providerError{}
	}
	return Grant{Handle: handle, TTL: ttl, Renewable: v.Renewable}, nil
}

func (p *VaultProvider) Revoke(ctx context.Context, handle string) error {
	if !p.handleValid(handle) {
		return &providerError{notSent: true}
	}
	b, status, err := p.request(ctx, http.MethodPost, "sys/leases/revoke", map[string]any{"lease_id": handle, "sync": true})
	clear(b)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return &providerError{}
	}
	return nil
}

func (p *VaultProvider) Lookup(ctx context.Context, handle string) (LeaseStatus, error) {
	if !p.handleValid(handle) {
		return LeaseStatus{}, &providerError{notSent: true}
	}
	b, status, err := p.request(ctx, http.MethodPost, "sys/leases/lookup", map[string]string{"lease_id": handle})
	if err != nil {
		return LeaseStatus{}, err
	}
	defer clear(b)
	if status == http.StatusBadRequest {
		var e struct {
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(b, &e) == nil && len(e.Errors) == 1 && e.Errors[0] == "invalid lease" {
			return LeaseStatus{Found: false}, nil
		}
	}
	var v struct {
		Data struct {
			ID        string `json:"id"`
			TTL       int64  `json:"ttl"`
			Renewable bool   `json:"renewable"`
		} `json:"data"`
	}
	if status != http.StatusOK || json.Unmarshal(b, &v) != nil || v.Data.ID != handle || v.Data.TTL < 0 || v.Data.TTL > 365*24*3600 {
		return LeaseStatus{}, &providerError{}
	}
	return LeaseStatus{Found: true, TTL: time.Duration(v.Data.TTL) * time.Second, Renewable: v.Data.Renewable}, nil
}
