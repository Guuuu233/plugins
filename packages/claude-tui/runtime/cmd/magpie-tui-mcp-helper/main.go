package main

import (
	"fmt"
	"os"

	"magpie-tui-runtime/internal/claudebridge"
)

// The helper is the deployed Magpie implementation, rehosted without requiring
// a forked Magpie executable. It delegates tool execution to its parent runtime.
func main() {
	if err := claudebridge.RunMCP(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
