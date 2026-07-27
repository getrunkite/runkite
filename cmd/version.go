package main

import "fmt"

// Set via -ldflags at build time.
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func cmdVersion() {
	fmt.Printf("runkite %s (commit: %s, built: %s)\n", Version, GitCommit, BuildTime)
}
