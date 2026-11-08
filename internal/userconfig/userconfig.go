// Package userconfig persists client UI preferences across difit sessions
// (~/.difit/config.json, or $DIFIT_CONFIG_DIR/config.json).
package userconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const configVersion = 1

// MaxBytes is a generous ceiling for UI preferences; anything larger is a bug
// or abuse.
const MaxBytes = 64 * 1024

type Config struct {
	Version int            `json:"version"`
	Client  map[string]any `json:"client"`
}

func Path() string {
	if dir := strings.TrimSpace(os.Getenv("DIFIT_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".difit", "config.json")
}

func decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func marshal(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParsePatch extracts the client settings from a PUT body of the form
// {"client": {...}}. It returns false for any other shape or oversize input.
func ParsePatch(body []byte) (map[string]any, bool) {
	v, err := decode(body)
	if err != nil {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	client, ok := obj["client"].(map[string]any)
	if !ok {
		return nil, false
	}
	encoded, err := marshal(client, false)
	if err != nil || len(encoded)-1 > MaxBytes {
		return nil, false
	}
	return client, true
}

// Read loads the config, falling back to defaults when it is missing or
// unreadable.
func Read(path string) Config {
	if raw, err := os.ReadFile(path); err == nil {
		if v, err := decode(raw); err == nil {
			if obj, ok := v.(map[string]any); ok {
				if client, ok := obj["client"].(map[string]any); ok {
					return Config{Version: configVersion, Client: client}
				}
			}
		}
	}
	return Config{Version: configVersion, Client: map[string]any{}}
}

// UpdateClient shallow-merges patch into the stored client settings.
// Concurrent difit servers may write the same file; settings changes are rare
// enough that last-write-wins per top-level key is acceptable.
func UpdateClient(patch map[string]any, path string) (Config, error) {
	next := Read(path)
	for k, v := range patch {
		next.Client[k] = v
	}
	serialized, err := marshal(next, true)
	if err != nil {
		return Config{}, err
	}
	if len(serialized) > MaxBytes {
		return Config{}, errors.New("User settings exceed the maximum allowed size")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Config{}, err
	}
	// Write via a temp file + rename so a crash mid-write can't corrupt the config.
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, serialized, 0o644); err != nil {
		return Config{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return Config{}, err
	}
	return next, nil
}

// Marshal encodes a config the way the API returns it.
func Marshal(c Config) ([]byte, error) {
	return marshal(c, false)
}
