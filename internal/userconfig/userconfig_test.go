package userconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathHonorsEnv(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom")
	t.Setenv("DIFIT_CONFIG_DIR", custom+" ")
	if got, want := Path(), filepath.Join(custom, "config.json"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	home := t.TempDir()
	t.Setenv("DIFIT_CONFIG_DIR", "  ")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got, want := Path(), filepath.Join(home, ".difit", "config.json"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if c := Read(path); c.Version != 1 || len(c.Client) != 0 {
		t.Fatalf("missing: %+v", c)
	}
	os.WriteFile(path, []byte("{not json"), 0o644)
	if c := Read(path); len(c.Client) != 0 {
		t.Fatalf("corrupt: %+v", c)
	}
	os.WriteFile(path, []byte(`{"version":7,"client":{"theme":"dark"}}`), 0o644)
	if c := Read(path); c.Version != 1 || c.Client["theme"] != "dark" {
		t.Fatalf("valid: %+v", c)
	}
}

func TestParsePatch(t *testing.T) {
	if p, ok := ParsePatch([]byte(`{"client":{"a":1}}`)); !ok || p["a"] == nil {
		t.Fatal("valid patch rejected")
	}
	for _, body := range []string{`{}`, `{"client":[]}`, `{"client":null}`, `[]`, `x`} {
		if _, ok := ParsePatch([]byte(body)); ok {
			t.Errorf("accepted %s", body)
		}
	}
	big := `{"client":{"a":"` + strings.Repeat("x", MaxBytes) + `"}}`
	if _, ok := ParsePatch([]byte(big)); ok {
		t.Error("accepted oversize patch")
	}
}

func TestUpdateClientMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	if _, err := UpdateClient(map[string]any{"a": 1, "b": "x"}, path); err != nil {
		t.Fatal(err)
	}
	c, err := UpdateClient(map[string]any{"b": "y"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Client) != 2 || c.Client["b"] != "y" {
		t.Fatalf("merged: %+v", c.Client)
	}
	raw, _ := os.ReadFile(path)
	want := "{\n  \"version\": 1,\n  \"client\": {\n    \"a\": 1,\n    \"b\": \"y\"\n  }\n}\n"
	if string(raw) != want {
		t.Fatalf("file:\n%s", raw)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}
