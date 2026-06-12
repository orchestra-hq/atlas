// Command atlas is the single entrypoint for the Atlas inference platform:
// control plane, worker, and operator CLI in one binary.
package main

import (
	"fmt"
	"os"

	"github.com/orchestra-hq/atlas/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "atlas:", err)
		os.Exit(1)
	}
}
