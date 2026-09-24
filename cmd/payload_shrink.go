package main

import (
	"log/slog"
	"os"

	goredis "github.com/redis/go-redis/v9"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/payloadshrink"
)

// initPayloadShrink reads payload_shrink from the first langgraph.json
// (same first-file convention as policy / finops). Invalid numbers exit
// 1. enabled without REDIS_URL logs once and leaves shrink as a no-op.
func initPayloadShrink(configPath string, rdb *goredis.Client) (payloadshrink.Settings, payloadshrink.Store) {
	var entry *config.PayloadShrinkEntry
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) > 0 {
		cfg, err := config.LoadLangGraphJSON(paths[0])
		if err != nil {
			slog.Error("payload_shrink: failed to load langgraph.json", "error", err)
			os.Exit(1)
		}
		entry = cfg.PayloadShrink
	}
	settings, err := config.ParsePayloadShrink(entry)
	if err != nil {
		slog.Error("payload_shrink: invalid config", "error", err)
		os.Exit(1)
	}
	if settings.Enabled && rdb == nil {
		slog.Warn("payload_shrink.enabled is true but REDIS_URL is unset; shrink is a no-op")
	}
	var store payloadshrink.Store
	if rdb != nil {
		store = payloadshrink.NewRedisStore(rdb)
	}
	return settings, store
}
