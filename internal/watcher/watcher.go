// Package watcher notifies connected browsers when the files behind the
// current diff change, over server-sent events.
package watcher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"difit-go/internal/types"
)

type modeConfig struct {
	watchPaths []string
	ignore     []string
}

var modeConfigs = map[types.DiffMode]modeConfig{
	types.DiffModeDefault: {
		watchPaths: []string{".git"},
		ignore:     []string{".git/objects/**", ".git/refs/**", "node_modules/**"},
	},
	types.DiffModeWorking: {
		watchPaths: []string{".", ".git"},
		ignore:     []string{".git/objects/**", ".git/refs/**", "node_modules/**"},
	},
	types.DiffModeStaged: {
		watchPaths: []string{".git"},
		ignore:     []string{".git/objects/**", ".git/refs/**"},
	},
	types.DiffModeDot: {
		watchPaths: []string{".", ".git"},
		ignore: []string{
			".git/objects/**",
			".git/refs/**",
			".git/FETCH_HEAD",
			".git/ORIG_HEAD",
			".git/logs/**",
			"node_modules/**",
		},
	},
	types.DiffModeSpecific: {},
}

// globToRegexp converts the simple glob patterns above into unanchored
// regular expressions matched against absolute slash-separated paths.
func globToRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case pattern[i] == '*':
			b.WriteString("[^/]*")
		default:
			// Dots are deliberately left unescaped, as in the original.
			b.WriteByte(pattern[i])
		}
	}
	return regexp.MustCompile(b.String())
}

type client struct {
	ch chan []byte
}

// Service owns the filesystem watches and the set of SSE clients.
type Service struct {
	stdout, stderr io.Writer

	mu           sync.Mutex
	clients      map[*client]struct{}
	mode         types.DiffMode
	started      bool
	repoPath     string
	debounce     time.Duration
	onInvalidate func()
	ignore       []*regexp.Regexp
	watchers     []*fsnotify.Watcher
	pending      map[string]bool // working-tree paths awaiting the gitignore check
	relevant     bool            // an event that needs no gitignore check arrived
	timer        *time.Timer
	done         chan struct{}
	wg           sync.WaitGroup
}

func New(stdout, stderr io.Writer) *Service {
	return &Service{stdout: stdout, stderr: stderr, clients: map[*client]struct{}{}}
}

// Start begins watching repoPath for changes relevant to mode. When a change
// settles for debounce, onInvalidate runs and clients receive a reload event.
func (s *Service) Start(mode types.DiffMode, repoPath string, debounce time.Duration, onInvalidate func()) error {
	s.stopWatching()

	s.mu.Lock()
	s.mode = mode
	s.started = true
	s.repoPath = repoPath
	s.debounce = debounce
	s.onInvalidate = onInvalidate
	s.done = make(chan struct{})
	s.pending = map[string]bool{}
	s.relevant = false
	s.mu.Unlock()

	if mode == types.DiffModeSpecific {
		fmt.Fprintln(s.stdout, "🔍 File watching disabled (specific commit comparison)")
		return nil
	}
	cfg, ok := modeConfigs[mode]
	if !ok {
		return fmt.Errorf("unknown diff mode %q", mode)
	}
	s.ignore = s.ignore[:0]
	for _, p := range cfg.ignore {
		s.ignore = append(s.ignore, globToRegexp(p))
	}

	for _, watchPath := range cfg.watchPaths {
		root := filepath.Join(repoPath, watchPath)
		if watchPath == ".git" {
			root = resolveGitDir(repoPath)
		}
		if err := s.watchRoot(root, watchPath == "."); err != nil {
			fmt.Fprintf(s.stderr, "⚠️  Could not watch %s: %v\n", root, err)
		}
	}
	return nil
}

// resolveGitDir finds the real git directory, which differs from .git in
// worktrees.
func resolveGitDir(repoPath string) string {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return filepath.Join(repoPath, ".git")
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir
}

func (s *Service) isIgnored(path string) bool {
	slash := filepath.ToSlash(path)
	for _, re := range s.ignore {
		if re.MatchString(slash) {
			return true
		}
	}
	return false
}

// isIgnoredDir reports whether everything under dir is ignored, so the
// directory need not be watched at all.
func (s *Service) isIgnoredDir(dir string) bool {
	return s.isIgnored(dir + string(filepath.Separator) + "x")
}

func (s *Service) watchRoot(root string, workingTree bool) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	var gitignored map[string]bool
	if workingTree {
		gitignored = s.ignoredDirs()
	}
	if err := s.addTree(w, root, workingTree, gitignored); err != nil {
		w.Close()
		return err
	}

	s.mu.Lock()
	s.watchers = append(s.watchers, w)
	done := s.done
	s.mu.Unlock()

	s.wg.Add(1)
	go s.loop(w, workingTree, done)
	return nil
}

// ignoredDirs lists gitignored directories so the initial walk can skip them.
func (s *Service) ignoredDirs() map[string]bool {
	cmd := exec.Command("git", "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Dir = s.repoPath
	out, err := cmd.Output()
	result := map[string]bool{}
	if err != nil {
		return result
	}
	for _, entry := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(entry, "/") {
			result[filepath.Join(s.repoPath, filepath.FromSlash(entry))] = true
		}
	}
	return result
}

func (s *Service) addTree(w *fsnotify.Watcher, root string, workingTree bool, gitignored map[string]bool) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (s.isIgnoredDir(path) || gitignored[path]) {
			return filepath.SkipDir
		}
		if err := w.Add(path); err != nil && path == root {
			return err
		}
		return nil
	})
}

func isRelevantGitFile(name string, mode types.DiffMode) bool {
	switch mode {
	case types.DiffModeDefault, types.DiffModeDot:
		return name == "HEAD"
	case types.DiffModeWorking, types.DiffModeStaged:
		return name == "index" || name == "HEAD"
	}
	return false
}

func (s *Service) loop(w *fsnotify.Watcher, workingTree bool, done <-chan struct{}) {
	defer s.wg.Done()
	for {
		select {
		case <-done:
			return
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(s.stderr, "Watch error: %v\n", err)
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			s.handleEvent(w, ev, workingTree)
		}
	}
}

func (s *Service) handleEvent(w *fsnotify.Watcher, ev fsnotify.Event, workingTree bool) {
	// A glob like node_modules/** also covers the directory itself, as in
	// @parcel/watcher, so creating node_modules is not a change either.
	if ev.Op == fsnotify.Chmod || s.isIgnored(ev.Name) || s.isIgnoredDir(ev.Name) {
		return
	}
	// Windows and kqueue also report a directory as written when entries
	// inside it change. Those entries have events of their own, or are
	// deliberately unwatched (.git internals, gitignored directories), so
	// the directory event adds nothing and would bypass the ignore rules.
	if ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			return
		}
	}
	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() && !s.isIgnoredDir(ev.Name) {
			if !workingTree || !s.checkIgnored([]string{ev.Name})[ev.Name] {
				s.addTree(w, ev.Name, workingTree, nil)
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if workingTree {
		s.pending[ev.Name] = true
	} else if isRelevantGitFile(filepath.Base(ev.Name), s.mode) {
		s.relevant = true
	} else {
		return
	}
	s.resetTimerLocked()
}

func (s *Service) resetTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
	}
	done := s.done
	s.timer = time.AfterFunc(s.debounce, func() { s.flush(done) })
}

// flush runs once events settle: it drops gitignored working-tree paths and,
// if anything relevant remains, invalidates caches and notifies clients.
func (s *Service) flush(done <-chan struct{}) {
	s.mu.Lock()
	if s.done != done {
		s.mu.Unlock()
		return
	}
	relevant := s.relevant
	paths := make([]string, 0, len(s.pending))
	for p := range s.pending {
		paths = append(paths, p)
	}
	s.pending = map[string]bool{}
	s.relevant = false
	s.mu.Unlock()

	if !relevant && len(paths) > 0 {
		ignored := s.checkIgnored(paths)
		for _, p := range paths {
			if !ignored[p] {
				relevant = true
				break
			}
		}
	}
	if !relevant {
		return
	}

	s.mu.Lock()
	onInvalidate, mode := s.onInvalidate, s.mode
	stopped := s.done != done
	s.mu.Unlock()
	if stopped {
		return
	}
	if onInvalidate != nil {
		onInvalidate()
	}
	s.Broadcast(types.WatchEvent{
		Type:       "reload",
		DiffMode:   mode,
		ChangeType: changeType(mode),
		Timestamp:  types.Now(),
		Message:    fmt.Sprintf("Changes detected in %s mode", mode),
	})
}

// checkIgnored returns the subset of absolute paths matched by .gitignore.
func (s *Service) checkIgnored(paths []string) map[string]bool {
	var input bytes.Buffer
	for _, p := range paths {
		rel, err := filepath.Rel(s.repoPath, p)
		if err != nil {
			rel = p
		}
		input.WriteString(filepath.ToSlash(rel))
		input.WriteByte(0)
	}
	cmd := exec.Command("git", "check-ignore", "--stdin", "-z")
	cmd.Dir = s.repoPath
	cmd.Stdin = &input
	// Exit status 1 means nothing matched; any output is still valid.
	out, _ := cmd.Output()
	result := map[string]bool{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			result[filepath.Join(s.repoPath, filepath.FromSlash(rel))] = true
		}
	}
	return result
}

func changeType(mode types.DiffMode) string {
	switch mode {
	case types.DiffModeDefault, types.DiffModeDot:
		return "commit" // HEAD changes indicate new commits
	case types.DiffModeStaged:
		return "staging" // index changes
	}
	return "file"
}

// Stop closes the filesystem watches and disconnects all clients.
func (s *Service) Stop() {
	s.stopWatching()
	s.mu.Lock()
	for c := range s.clients {
		close(c.ch)
	}
	s.clients = map[*client]struct{}{}
	s.mu.Unlock()
}

func (s *Service) stopWatching() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	watchers := s.watchers
	s.watchers = nil
	s.mu.Unlock()

	for _, w := range watchers {
		w.Close()
	}
	s.wg.Wait()
}

func encodeEvent(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return []byte("data: " + strings.TrimSuffix(buf.String(), "\n") + "\n\n")
}

// Subscribe registers an SSE client. The returned channel yields encoded
// events, starting with a connected event, and is closed on Stop.
func (s *Service) Subscribe() (<-chan []byte, func()) {
	c := &client{ch: make(chan []byte, 64)}
	s.mu.Lock()
	mode := s.mode
	modeLabel := string(mode)
	if !s.started {
		mode = types.DiffModeDefault
		modeLabel = "undefined"
	}
	s.clients[c] = struct{}{}
	c.ch <- encodeEvent(types.WatchEvent{
		Type:       "connected",
		DiffMode:   mode,
		ChangeType: "file",
		Timestamp:  types.Now(),
		Message:    fmt.Sprintf("Connected to file watcher (%s mode)", modeLabel),
	})
	s.mu.Unlock()

	return c.ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.clients[c]; ok {
			delete(s.clients, c)
			close(c.ch)
		}
	}
}

// Broadcast sends an event to every client. Clients too slow to keep up
// miss the event rather than blocking the sender.
func (s *Service) Broadcast(ev types.WatchEvent) {
	data := encodeEvent(ev)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		select {
		case c.ch <- data:
		default:
		}
	}
}
