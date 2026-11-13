package main

import (
	"context"
	"errors"

	"github.com/getrunkite/runkite/internal/actionexport"
	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/state"
)

func initActionExport(ctx context.Context, path string, store state.Store, server *api.Server) error {
	paths := config.FindLangGraphJSON(path)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil {
		return err
	}
	if cfg.Policy == nil || cfg.Policy.ActionExport == nil {
		return nil
	}
	if !cfg.Policy.ActionEvidence || !cfg.Policy.AuditRequired {
		return errors.New("action_export requires SQL action evidence and required audit")
	}
	ledger, ok := store.(state.ActionDeliveryStore)
	if !ok {
		return errors.New("action_export requires durable SQL outbox storage")
	}
	entry := cfg.Policy.ActionExport
	worker, err := actionexport.New(ledger, actionexport.Config{Name: entry.Name, URL: entry.URL, SecretRef: entry.SecretRef, MaxAttempts: entry.MaxAttempts, AllowInsecureLoopback: entry.AllowInsecureLoopback})
	if err != nil {
		return errors.New("invalid action export destination")
	}
	server.SetActionExportSinkID(worker.SinkID())
	go worker.Run(ctx)
	return nil
}
