// Package actionexport delivers committed action evidence, never tool requests.
package actionexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"time"

	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

var ErrUnavailable = errors.New("action event delivery unavailable")

type Config struct {
	Name, URL, SecretRef  string
	AllowInsecureLoopback bool
	MaxAttempts           int
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// SinkID binds the logical name and exact registered destination. Changing URL
// creates another queue identity, so an old queue cannot be silently rerouted.
func (c Config) SinkID() (string, error) {
	if !namePattern.MatchString(c.Name) {
		return "", ErrUnavailable
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrUnavailable
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !c.AllowInsecureLoopback || ip == nil || !ip.IsLoopback() {
			return "", ErrUnavailable
		}
	}
	if c.MaxAttempts < 0 || c.MaxAttempts > 100 {
		return "", ErrUnavailable
	}
	raw, _ := json.Marshal(struct{ Name, URL, Schema string }{c.Name, c.URL, "runkite.action-event.v1"})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type Worker struct {
	store  state.ActionDeliveryStore
	cfg    Config
	sinkID string
	now    func() time.Time
}

func New(store state.ActionDeliveryStore, cfg Config) (*Worker, error) {
	id, err := cfg.SinkID()
	if err != nil || store == nil {
		return nil, ErrUnavailable
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 10
	}
	return &Worker{store: store, cfg: cfg, sinkID: id, now: func() time.Time { return time.Now().UTC() }}, nil
}
func (w *Worker) SinkID() string { return w.sinkID }
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_, _ = w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	ctx = tenant.SystemContext(ctx)
	now := w.now()
	rows, err := w.store.ClaimActionDeliveries(ctx, w.sinkID, now, now.Add(30*time.Second), 1)
	if err != nil {
		return false, ErrUnavailable
	}
	if len(rows) == 0 {
		return false, nil
	}
	row := rows[0]
	reason := "export_endpoint_unavailable"
	var secret string
	if w.cfg.SecretRef != "" {
		secret, err = connector.ResolveSecretRef(ctx, w.cfg.SecretRef)
		if err != nil {
			reason = "export_signing_unavailable"
		}
	}
	if err == nil {
		sink := hooks.NewWebhookSink(hooks.WebhookConfig{URL: w.cfg.URL, Secret: secret}, nil)
		err = sink.DeliverOnce(ctx, row.PayloadRaw, row.ID)
	}
	if err == nil {
		if err := w.store.AckActionDelivery(ctx, row.ID, row.ClaimToken); err != nil {
			return true, ErrUnavailable
		}
		return true, nil
	}
	delay := time.Second << min(row.Attempts, 10)
	if err := w.store.FailActionDelivery(ctx, row.ID, row.ClaimToken, w.now().Add(delay), reason, row.Attempts >= w.cfg.MaxAttempts); err != nil {
		return true, ErrUnavailable
	}
	return true, ErrUnavailable
}
