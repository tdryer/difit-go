// Command difit serves a GitHub-style review UI for git diffs.
package main

import (
	"os"

	"github.com/tdryer/difit-go/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], version))
}
