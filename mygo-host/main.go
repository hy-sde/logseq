// Command mygo-host is a Go desktop host for the Logseq renderer,
// built on the mygo framework. Milestone 1: window + IPC + filesystem +
// graph lifecycle + db-worker daemon management.
//
// Usage:
//
//	LOGSEQ_RESOURCES=/path/to/static ./mygo-host
//
// The static/ directory must be built first (`pnpm gulp` in the logseq
// repo): it holds index.html, the built renderer JS, electron shim
// assets and db-worker-node.js (see README.md).
package main

import (
	"log"
	"os"

	"logseq/mygo-host/internal/host"
)

func main() {
	if err := host.Run(); err != nil {
		log.Printf("mygo-host: %v", err)
		os.Exit(1)
	}
}
