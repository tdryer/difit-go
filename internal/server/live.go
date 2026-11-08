package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tdryer/difit-go/internal/editor"
	"github.com/tdryer/difit-go/internal/proc"
)

func launchEditor(command string, args []string) error {
	cmd := exec.Command(command, args...)
	return proc.StartDetached(cmd)
}

// parseLeadingInt mimics Number.parseInt(String(v), 10) for JSON values,
// returning 0 when no integer can be read.
func parseLeadingInt(v any) int {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case fmt.Stringer:
		s = x.String()
	default:
		return 0
	}
	s = strings.TrimLeft(s, " \t\n\r\v\f")
	sign := 1
	if s != "" && (s[0] == '+' || s[0] == '-') {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	n := 0
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		if n > 1<<30 {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	return sign * n
}

func lookupEditorID(obj map[string]any) (string, bool) {
	if editorObj, ok := obj["editor"].(map[string]any); ok {
		if id, ok := editorObj["id"].(string); ok {
			return id, true
		}
	}
	if id, ok := os.LookupEnv("DIFIT_EDITOR"); ok {
		return id, true
	}
	return os.LookupEnv("EDITOR")
}

func (s *Server) handleOpenInEditor(w http.ResponseWriter, r *http.Request) {
	if s.stdin {
		writeError(w, http.StatusBadRequest, "Open in editor is not available for stdin diff")
		return
	}
	body, err := readBody(w, r)
	var obj map[string]any
	if err == nil {
		if v, err := decodeJSON(body); err == nil {
			obj, _ = v.(map[string]any)
		}
	}
	filePath, ok := obj["filePath"].(string)
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	path, ok := s.repoRelativePath(w, filePath)
	if !ok {
		return
	}
	resolvedPath := filepath.Join(s.repoPath, filepath.FromSlash(path))

	editorID, hasEditorID := lookupEditorID(obj)
	if hasEditorID && strings.ToLower(editorID) == editor.NoneID {
		writeError(w, http.StatusBadRequest, "Open in editor is disabled")
		return
	}

	// The browser always sends command + argsTemplate; the preset table is
	// only used when neither is provided (e.g. DIFIT_EDITOR with no body).
	editorObj, _ := obj["editor"].(map[string]any)
	reqCommand, hasCommand := editorObj["command"].(string)
	reqArgs, hasArgs := editorObj["argsTemplate"].(string)
	var command, argsTemplate string
	if hasCommand || hasArgs {
		command, argsTemplate = strings.TrimSpace(reqCommand), strings.TrimSpace(reqArgs)
	} else {
		preset := editor.Resolve(editorID)
		command, argsTemplate = preset.Command, preset.ArgsTemplate
	}
	if command == "" || argsTemplate == "" {
		message := "Open in editor is not configured"
		if hasEditorID && strings.ToLower(editorID) == editor.CustomID {
			message = "Custom editor is not configured. Set a command and arguments in Settings > System."
		}
		writeError(w, http.StatusBadRequest, message)
		return
	}

	args := editor.BuildArgs(argsTemplate, resolvedPath, parseLeadingInt(obj["line"]))
	launch := s.opts.LaunchEditor
	if launch == nil {
		launch = launchEditor
	}
	if err := launch(command, args); err != nil {
		if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
			s.logError("Failed to launch editor CLI:", err)
		}
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("Failed to launch editor: command %q is not available on PATH", command))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func startSSE(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	flusher, ok := startSSE(w)
	if !ok {
		return
	}
	events, cancel := s.watcher.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-events:
			if !ok {
				return
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleHeartbeat lets the server notice when the browser tab closes.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	flusher, ok := startSSE(w)
	if !ok {
		return
	}
	w.Write([]byte("data: connected\n\n"))
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.Write([]byte("data: heartbeat\n\n"))
			flusher.Flush()
		case <-s.shutdown:
			return
		case <-r.Context().Done():
			s.clientDisconnected()
			return
		}
	}
}

func (s *Server) clientDisconnected() {
	if s.opts.KeepAlive {
		fmt.Fprintln(s.opts.Stdout, "Client disconnected, but server is staying alive (--keep-alive)")
		fmt.Fprintln(s.opts.Stdout, "Press Ctrl+C to stop the server")
		return
	}
	// A short delay lets pending sendBeacon requests land first.
	time.AfterFunc(disconnectGrace, func() {
		s.shutdownOnce.Do(func() {
			fmt.Fprintln(s.opts.Stdout, "Client disconnected, shutting down server...")
			close(s.shutdown)
		})
	})
}
