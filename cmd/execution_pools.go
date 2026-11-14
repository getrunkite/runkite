package main

import (
	"fmt"
	"reflect"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/execution"
)

// loadExecutionPools treats all discovered pool definitions as operator-owned
// startup configuration. Conflicting replicas/config files must not broaden it.
func loadExecutionPools(path string) (*execution.Catalog, error) {
	definitions := map[string]execution.Pool{}
	var configs []*config.LangGraphConfig
	for _, file := range config.FindLangGraphJSON(path) {
		cfg, err := config.LoadLangGraphJSON(file)
		if err != nil {
			return nil, fmt.Errorf("execution pool configuration: %w", err)
		}
		configs = append(configs, cfg)
		for name, pool := range cfg.ExecutionPools {
			if old, ok := definitions[name]; ok && !reflect.DeepEqual(old, pool) {
				return nil, fmt.Errorf("conflicting execution pool definition %q", name)
			}
			definitions[name] = pool
		}
	}
	catalog, err := execution.NewCatalog(definitions)
	if err != nil {
		return nil, err
	}
	for _, cfg := range configs {
		if cfg.ExecutionPool == "" {
			if execution.IsPoolKind(cfg.RunnerKind) {
				return nil, fmt.Errorf("reserved runner_kind requires execution_pool selection")
			}
			continue
		}
		var agents []string
		for id := range cfg.Graphs {
			agents = append(agents, id)
		}
		if _, err := catalog.Select(cfg.ExecutionPool, cfg.RunnerKind, agents); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func loadPoolRunnerTokens(path string) (*auth.RunnerTokens, *execution.Catalog, error) {
	catalog, err := loadExecutionPools(path)
	if err != nil {
		return nil, nil, err
	}
	tokens := auth.LoadRunnerTokensFromEnv()
	if err := tokens.ConfigureExecutionPools(catalog); err != nil {
		return nil, nil, err
	}
	return tokens, catalog, nil
}
