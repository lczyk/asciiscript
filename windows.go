//go:build windows

package main

import (
	"fmt"
	"os"
)

// The recording is hosted in a Unix pty, which Windows hasn't got.
func main() {
	fmt.Fprintln(os.Stderr, "asciiscript needs a Unix pty and doesn't run on Windows -- try WSL")
	os.Exit(1)
}
