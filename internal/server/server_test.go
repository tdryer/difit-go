package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesIndex(t *testing.T) {
	static := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>hello")}}
	srv := httptest.NewServer(NewHandler(static))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hello") {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestListenFallsBackWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	var logged string
	ln, url, err := Listen("127.0.0.1", busyPort, func(format string, _ ...any) {
		logged = format
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	gotPort := ln.Addr().(*net.TCPAddr).Port
	if gotPort == busyPort {
		t.Fatalf("expected a port other than %d", busyPort)
	}
	if !strings.HasSuffix(url, ":"+strconv.Itoa(gotPort)) {
		t.Fatalf("url %q does not match port %d", url, gotPort)
	}
	if logged == "" {
		t.Fatal("expected busy-port message")
	}
}

func TestListenDisplaysLocalhostForWildcard(t *testing.T) {
	ln, url, err := Listen("0.0.0.0", 0, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if url != "http://localhost:0" {
		t.Fatalf("got %q", url)
	}
}
