package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func initCredentialLeases(ctx context.Context, path string, store state.Store, server *api.Server, registry *connector.Registry) error {
	paths := config.FindLangGraphJSON(path)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil {
		return err
	}
	if cfg.CredentialLeases == nil {
		if registry != nil {
			for _, name := range registry.List() {
				c, _ := registry.Get(name)
				if c.Config.Auth.Type == "vault_database" {
					return errors.New("vault_database requires credential_leases configuration")
				}
			}
		}
		return nil
	}
	if cfg.Policy == nil || !cfg.Policy.AuditRequired || !cfg.Policy.ActionEvidence || registry == nil {
		return errors.New("credential_leases requires SQL action evidence and required audit")
	}
	provider, ok := store.(interface {
		CredentialLeaseStorage() (credentials.Store, error)
	})
	if !ok {
		return errors.New("credential_leases requires SQL storage")
	}
	ledger, err := provider.CredentialLeaseStorage()
	if err != nil {
		return err
	}
	resolve := func(value string) string {
		if filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(filepath.Dir(paths[0]), value)
	}
	keys, err := credentials.LoadKeyring(resolve(cfg.CredentialLeases.KeyringFile))
	if err != nil {
		return errors.New("credential handle keyring unavailable")
	}
	var bindings []credentials.Binding
	active := map[string]string{}
	for _, entry := range cfg.CredentialLeases.Bindings {
		vp, err := credentials.NewVaultProvider(credentials.VaultConfig{Address: entry.Address, Namespace: entry.Namespace, TokenFile: resolve(entry.TokenFile), Mount: entry.Mount, Role: entry.Role, AllowInsecureLoopback: entry.AllowInsecureLoopback})
		if err != nil {
			return errors.New("invalid registered credential issuer")
		}
		maxTTL, renew := time.Duration(entry.MaxTTLSeconds)*time.Second, time.Duration(entry.RenewSeconds)*time.Second
		revision := vp.Revision(entry.ConsumerID, maxTTL, renew)
		bindings = append(bindings, credentials.Binding{ID: entry.ID, Revision: revision, ConsumerID: entry.ConsumerID, MaxTTL: maxTTL, RenewIncrement: renew, Provider: vp, Retired: entry.Retired})
		if !entry.Retired {
			c, err := registry.Get(entry.ConsumerID)
			if err != nil || c.Config.Auth.Type != "vault_database" || c.Config.Auth.CredentialBinding != entry.ID {
				return errors.New("credential binding must name its registered database MCP consumer")
			}
			if _, exists := active[entry.ConsumerID]; exists {
				return errors.New("duplicate active credential consumer")
			}
			active[entry.ConsumerID] = revision
			c.Config.Auth.CredentialBindingRevision = revision
		}
	}
	manager, err := credentials.NewManager(ledger, keys, bindings, server.CheckCredentialOwner)
	if err != nil {
		return fmt.Errorf("invalid credential lifecycle configuration: %w", err)
	}
	for _, name := range registry.List() {
		c, _ := registry.Get(name)
		if c.Config.Auth.Type == "vault_database" && active[name] == "" {
			return errors.New("database MCP connector has no active registered credential binding")
		}
	}
	server.SetCredentialManager(manager)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := manager.Reconcile(tenant.SystemContext(ctx), 100); err != nil {
					slog.Warn("credential cleanup reconciliation incomplete")
				}
			}
		}
	}()
	return nil
}
