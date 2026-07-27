package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/sharanharsoor/runkite/internal/config"
)

func cmdAgentsList(args []string) {
	// Strip "list" from args if present (handles both "agents list" and bare "agents")
	if len(args) > 0 && args[0] == "list" {
		args = args[1:]
	}

	fs := flag.NewFlagSet("agents list", flag.ExitOnError)
	configPath := fs.String("config", "", "path to langgraph.json")
	fs.StringVar(configPath, "c", "", "path to langgraph.json (shorthand)")
	fs.Parse(args)

	cfgPath := resolveConfig(*configPath)
	paths := config.FindLangGraphJSON(cfgPath)
	if len(paths) == 0 {
		fmt.Println("No langgraph.json found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT_ID\tSOURCE\tSYMBOL")
	fmt.Fprintln(w, "--------\t------\t------")

	for _, path := range paths {
		cfg, err := config.LoadLangGraphJSON(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
			continue
		}
		entries, err := cfg.ParseGraphEntries()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, err)
			continue
		}
		for _, entry := range entries {
			fmt.Fprintf(w, "%s\t%s\t%s\n", entry.GraphID, path, entry.Symbol)
		}
	}
	w.Flush()
}
