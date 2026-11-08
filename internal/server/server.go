// Package server hosts the difit HTTP API and the embedded web client.
package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"syscall"
)

// DefaultPort is the first port tried when none is requested.
const DefaultPort = 4966

// NewHandler returns the root handler serving the web client from static.
func NewHandler(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}

// Listen binds host:port, moving to the next port while the current one is in
// use. It returns the listener and the URL to show the user.
func Listen(host string, port int, logf func(format string, args ...any)) (net.Listener, string, error) {
	for {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			displayHost := host
			if host == "0.0.0.0" {
				displayHost = "localhost"
			}
			return ln, fmt.Sprintf("http://%s:%d", displayHost, port), nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, "", fmt.Errorf("Failed to launch a server: %w", err)
		}
		logf("Port %d is busy, trying %d...\n", port, port+1)
		port++
	}
}
