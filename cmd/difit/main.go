// Command difit serves a GitHub-like review UI for Git diffs.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"difit-go/internal/server"
	"difit-go/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Placeholder entrypoint: the full CLI lands with the CLI milestone.
	port := flag.Int("port", server.DefaultPort, "preferred port (auto-assigned if occupied)")
	host := flag.String("host", "localhost", "host address to bind")
	showVersion := flag.Bool("version", false, "output the version number")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	ln, url, err := server.Listen(*host, *port, func(format string, args ...any) {
		fmt.Printf(format, args...)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Printf("\n🚀 difit server started on %s\n", url)
	if err := http.Serve(ln, server.NewHandler(web.FS())); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
