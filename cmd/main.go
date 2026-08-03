// Runkite control plane CLI entrypoint.
// Dispatches to subcommands using os.Args — no external CLI frameworks.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "dev":
		cmdDev(os.Args[2:])
	case "run", "serve":
		cmdServe(os.Args[2:])
	case "db":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: runkite db <upgrade|downgrade|reset>")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "upgrade":
			cmdDBUpgrade(os.Args[3:])
		case "downgrade":
			cmdDBDowngrade(os.Args[3:])
		case "reset":
			cmdDBReset(os.Args[3:])
		default:
			fmt.Fprintf(os.Stderr, "unknown db command: %s\n", os.Args[2])
			os.Exit(1)
		}
	case "agents":
		if len(os.Args) < 3 || os.Args[2] == "list" {
			cmdAgentsList(os.Args[2:])
		} else {
			fmt.Fprintf(os.Stderr, "unknown agents command: %s\n", os.Args[2])
			os.Exit(1)
		}
	case "version":
		cmdVersion()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`Usage: runkite <command> [options]

Commands:
  dev             Start control plane in development mode (auto-discovers agents)
  run, serve      Start control plane in production mode
  db upgrade      Apply pending numbered schema migrations
  db downgrade    Roll back the most recently applied migration
  db reset        Reset database (truncate/recreate + upgrade to latest)
  agents list     List registered agents from config
  version         Print version info
  help            Show this help message

Use "runkite <command> --help" for more information about a command.
`)
}
