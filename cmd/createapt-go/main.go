// Command createapt-go creates, updates, publishes and validates apt
// (Debian/Ubuntu) package repositories on local disk or remote storage.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
