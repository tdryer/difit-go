// Package server hosts the difit HTTP API and the embedded web client.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"difit-go/internal/comments"
	"difit-go/internal/gitdiff"
	"difit-go/internal/types"
	"difit-go/internal/userconfig"
	"difit-go/internal/watcher"
)

// DefaultPort is the first port tried when none is requested.
const DefaultPort = 4966

const (
	generatedStatusCacheTTL = 60 * time.Second
	maxDiffCacheEntries     = 8
	maxBodyBytes            = 10 << 20
	watchDebounce           = 300 * time.Millisecond
	heartbeatInterval       = 5 * time.Second
	disconnectGrace         = 100 * time.Millisecond
)

type Options struct {
	Selection        types.DiffSelection
	StdinDiff        string // non-empty serves this diff instead of reading git
	PreferredPort    int
	Host             string
	OpenBrowser      bool
	IgnoreWhitespace bool
	ClearComments    bool
	CommentImports   []types.CommentImport
	KeepAlive        bool
	DiffMode         types.DiffMode // empty disables the file watcher
	RepoPath         string
	ContextLines     int // negative means git's default
	Static           fs.FS

	Stdout, Stderr io.Writer
	// OpenURL opens the browser; nil disables it.
	OpenURL func(url string) error
	// ConfigPath overrides the user settings location (for tests).
	ConfigPath string
	// LaunchEditor starts an editor process; defaults to a detached exec.
	LaunchEditor func(command string, args []string) error
}

type generatedCacheEntry struct {
	value     types.GeneratedStatusResponse
	expiresAt time.Time
}

type diffCacheEntry struct {
	key   string
	value types.DiffResponse
}

type commentSession struct {
	threads []types.DiffCommentThread
	version int
}

// Server holds the state behind the API.
type Server struct {
	opts            Options
	stdin           bool
	repoPath        string
	repositoryID    string
	commentImportID string
	parser          *gitdiff.Parser
	watcher         *watcher.Service
	initialDiff     types.DiffResponse
	handler         http.Handler

	mu                      sync.Mutex
	diffCache               []diffCacheEntry // most recently used last
	generatedCache          map[string]generatedCacheEntry
	currentSelection        types.DiffSelection
	currentCommentSelection types.DiffSelection
	commentSessions         map[string]*commentSession

	httpServer   *http.Server
	Port         int
	URL          string
	shutdownOnce sync.Once
	shutdown     chan struct{}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// New validates the selection, computes the initial diff and builds the
// handler, without listening.
func New(opts Options) (*Server, error) {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	repoPath, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:            opts,
		stdin:           opts.StdinDiff != "",
		repoPath:        repoPath,
		repositoryID:    sha256Hex(repoPath),
		parser:          gitdiff.NewParser(repoPath),
		watcher:         watcher.New(opts.Stdout, opts.Stderr),
		generatedCache:  map[string]generatedCacheEntry{},
		commentSessions: map[string]*commentSession{},
		shutdown:        make(chan struct{}),
	}
	if len(opts.CommentImports) > 0 {
		s.commentImportID = comments.ImportID(opts.CommentImports)
	}

	if s.stdin {
		s.initialDiff = gitdiff.ParseStdinDiff(opts.StdinDiff)
	} else {
		if !s.parser.ValidateCommit(opts.Selection.TargetCommitish) {
			return nil, fmt.Errorf("Invalid or non-existent commit: %s", opts.Selection.TargetCommitish)
		}
		s.initialDiff, err = s.parser.ParseDiff(opts.Selection, opts.IgnoreWhitespace, opts.ContextLines)
		if err != nil {
			return nil, err
		}
		s.setCachedDiff(diffCacheKey(opts.Selection, opts.IgnoreWhitespace), s.initialDiff)
	}

	s.currentSelection = opts.Selection
	s.currentCommentSelection = s.resolvedCommentSelection(s.initialDiff, opts.Selection)
	if threads := comments.MergeImports(nil, opts.CommentImports).Threads; len(threads) > 0 {
		s.commentSessions[s.currentCommentSelection.Key()] = &commentSession{threads: threads, version: 1}
	}

	s.handler = s.routes()
	return s, nil
}

// Start creates the server, listens, and starts the watcher and browser.
func Start(opts Options) (*Server, error) {
	s, err := New(opts)
	if err != nil {
		return nil, err
	}
	host := opts.Host
	if host == "" {
		host = "localhost"
	}
	port := opts.PreferredPort
	if port == 0 {
		port = DefaultPort
	}
	ln, url, err := Listen(host, port, func(format string, args ...any) {
		fmt.Fprintf(s.opts.Stdout, format, args...)
	})
	if err != nil {
		return nil, err
	}
	s.Port = ln.Addr().(*net.TCPAddr).Port
	s.URL = url
	s.httpServer = &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	go s.httpServer.Serve(ln)

	if opts.Host != "" && opts.Host != "127.0.0.1" && opts.Host != "localhost" {
		fmt.Fprintln(s.opts.Stderr, "\n⚠️  WARNING: Server is accessible from external network!")
		fmt.Fprintf(s.opts.Stderr, "   Binding to: %s:%d\n", opts.Host, s.Port)
		fmt.Fprintln(s.opts.Stderr, "   Make sure this is intended and your network is secure.")
		fmt.Fprintln(s.opts.Stderr)
	}

	if opts.DiffMode != "" {
		if err := s.watcher.Start(opts.DiffMode, s.repoPath, watchDebounce, s.invalidateCache); err != nil {
			fmt.Fprintln(s.opts.Stderr, "⚠️  File watcher failed to start:", err)
			fmt.Fprintln(s.opts.Stderr, "   Continuing without file watching...")
		}
	}

	if !s.initialDiff.IsEmpty && opts.OpenBrowser && opts.OpenURL != nil {
		if err := opts.OpenURL(url); err != nil {
			fmt.Fprintln(s.opts.Stderr, "Failed to open browser automatically")
		}
	}
	return s, nil
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.handler }

// IsEmpty reports whether the initial diff had no changes.
func (s *Server) IsEmpty() bool { return s.initialDiff.IsEmpty }

// ShutdownRequested is closed when the browser tab goes away and the server
// is not in keep-alive mode.
func (s *Server) ShutdownRequested() <-chan struct{} { return s.shutdown }

// Close stops the file watcher and the HTTP server.
func (s *Server) Close() {
	s.watcher.Stop()
	if s.httpServer != nil {
		s.httpServer.Close()
	}
}

// FinalComments renders the comments for the current selection, or "" when
// there are none.
func (s *Server) FinalComments() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessionLocked(s.currentCommentSelection)
	if len(session.threads) == 0 {
		return ""
	}
	return comments.FormatOutput(session.threads)
}

func (s *Server) invalidateCache() {
	s.mu.Lock()
	s.diffCache = nil
	clear(s.generatedCache)
	s.mu.Unlock()
	s.parser.ClearResolvedCommitCache()
}

func diffCacheKey(sel types.DiffSelection, ignoreWhitespace bool) string {
	flag := "0"
	if ignoreWhitespace {
		flag = "1"
	}
	return sel.Key() + "\x00" + flag
}

func (s *Server) getCachedDiffLocked(key string) (types.DiffResponse, bool) {
	for i, e := range s.diffCache {
		if e.key == key {
			s.diffCache = append(append(s.diffCache[:i:i], s.diffCache[i+1:]...), e)
			return e.value, true
		}
	}
	return types.DiffResponse{}, false
}

func (s *Server) setCachedDiff(key string, value types.DiffResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCachedDiffLocked(key, value)
}

func (s *Server) setCachedDiffLocked(key string, value types.DiffResponse) {
	for i, e := range s.diffCache {
		if e.key == key {
			s.diffCache = append(s.diffCache[:i:i], s.diffCache[i+1:]...)
			break
		}
	}
	s.diffCache = append(s.diffCache, diffCacheEntry{key, value})
	if n := len(s.diffCache); n > maxDiffCacheEntries {
		s.diffCache = s.diffCache[n-maxDiffCacheEntries:]
	}
}

func (s *Server) resolvedCommentSelection(resp types.DiffResponse, fallback types.DiffSelection) types.DiffSelection {
	base, target := resp.BaseCommitish, resp.TargetCommitish
	if base == "" {
		base = fallback.BaseCommitish
		if s.stdin {
			base = "stdin"
		}
	}
	if target == "" {
		target = fallback.TargetCommitish
		if s.stdin {
			target = "stdin"
		}
	}
	mode := resp.RequestedBaseMode
	if mode == "" {
		mode = fallback.BaseMode
	}
	return types.NewDiffSelection(base, target, mode)
}

// selectionFromQuery applies base/target/baseMode query parameters to
// current. Naming base or target without baseMode resets it to direct.
func selectionFromQuery(q map[string][]string, current types.DiffSelection) types.DiffSelection {
	base, hasBase := q["base"]
	target, hasTarget := q["target"]
	baseMode, hasBaseMode := q["baseMode"]
	if !hasBase && !hasTarget && !hasBaseMode {
		return current
	}
	sel := current
	if hasBase {
		sel.BaseCommitish = base[0]
	}
	if hasTarget {
		sel.TargetCommitish = target[0]
	}
	mode := current.BaseMode
	if hasBaseMode {
		mode = baseMode[0]
	} else if hasBase || hasTarget {
		mode = ""
	}
	return types.NewDiffSelection(sel.BaseCommitish, sel.TargetCommitish, mode)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := comments.MarshalJSON(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}

func (s *Server) logError(context string, err error) {
	fmt.Fprintf(s.opts.Stderr, "%s %v\n", context, err)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/diff", s.handleDiff)
	mux.HandleFunc("GET /api/generated-status/{path...}", s.handleGeneratedStatus)
	mux.HandleFunc("GET /api/revisions", s.handleRevisions)
	mux.HandleFunc("GET /api/line-count/{path...}", s.handleLineCount)
	mux.HandleFunc("GET /api/blob/{path...}", s.handleBlob)
	mux.HandleFunc("POST /api/comments", s.handlePostComments)
	mux.HandleFunc("POST /api/comment-imports", s.handleCommentImports)
	mux.HandleFunc("DELETE /api/comments/{threadId}", s.handleDeleteComment)
	mux.HandleFunc("GET /api/comments-json", s.handleCommentsJSON)
	mux.HandleFunc("GET /api/comments-output", s.handleCommentsOutput)
	mux.HandleFunc("GET /api/user-settings", s.handleGetUserSettings)
	mux.HandleFunc("PUT /api/user-settings", s.handlePutUserSettings)
	mux.HandleFunc("POST /api/open-in-editor", s.handleOpenInEditor)
	mux.HandleFunc("GET /api/watch", s.handleWatch)
	mux.HandleFunc("GET /api/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if s.opts.Static != nil {
		mux.Handle("GET /", http.FileServerFS(s.opts.Static))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "http://localhost:*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
		mux.ServeHTTP(w, r)
	})
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

func (s *Server) handleGetUserSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userconfig.Read(s.configPath()))
}

func (s *Server) handlePutUserSettings(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	var patch map[string]any
	ok := err == nil
	if ok {
		patch, ok = userconfig.ParsePatch(body)
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid user settings payload")
		return
	}
	config, err := userconfig.UpdateClient(patch, s.configPath())
	if err != nil {
		s.logError("Error saving user settings:", err)
		writeError(w, http.StatusInternalServerError, "Failed to save user settings")
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) configPath() string {
	if s.opts.ConfigPath != "" {
		return s.opts.ConfigPath
	}
	return userconfig.Path()
}

// decodeJSON decodes a request body, preserving number literals.
func decodeJSON(body []byte) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
