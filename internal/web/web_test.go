package web

import (
	"io/fs"
	"testing"
)

func TestFSHasIndex(t *testing.T) {
	if _, err := fs.Stat(FS(), "index.html"); err != nil {
		t.Fatalf("index.html missing: %v", err)
	}
}
