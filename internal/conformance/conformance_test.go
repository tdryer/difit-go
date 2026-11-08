// Package conformance checks that the Go server answers its HTTP API and
// watch stream exactly like upstream difit v5.0.12's Node server.
//
// By default the Go binary's responses are compared with responses recorded
// from the Node server in testdata/*.json. With DIFIT_REFERENCE set to an
// upstream build's dist/cli/index.js, both servers run side by side instead,
// and -update re-records testdata from the Node server:
//
//	DIFIT_REFERENCE=$PWD/difit/dist/cli/index.js go test ./internal/conformance -update
package conformance

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tdryer/difit-go/internal/testrepo"
)

var update = flag.Bool("update", false, "re-record testdata from the reference server (needs DIFIT_REFERENCE)")

// A step is one thing a scenario does to every server under test; run
// returns one normalized result per instance.
type step interface {
	label() string
	run(t *testing.T, insts []*instance) []any
}

type request struct {
	method, path, body string
	// statusOnly skips body comparison where the implementations
	// knowingly differ (see the call sites).
	statusOnly bool
}

func get(path string) request { return request{method: "GET", path: path} }

func post(path, body string) request { return request{method: "POST", path: path, body: body} }

func (r request) label() string { return r.method + " " + r.path }

func (r request) run(t *testing.T, insts []*instance) []any {
	var results []any
	for _, inst := range insts {
		results = append(results, inst.do(t, r))
	}
	return results
}

// watchStep applies an action to each instance's repository, then records
// the /api/watch events that follow until the stream goes quiet.
type watchStep struct {
	name   string
	action func(r *testrepo.Repo)
}

func (w watchStep) label() string { return "watch: " + w.name }

func (w watchStep) run(t *testing.T, insts []*instance) []any {
	if w.action != nil {
		for _, inst := range insts {
			w.action(inst.repo)
		}
	}
	results := make([]any, len(insts))
	var wg sync.WaitGroup
	for i, inst := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = inst.normalize(toAny(collectEvents(inst.events)))
		}()
	}
	wg.Wait()
	return results
}

// collectEvents waits up to 3s for the first event, then until no event
// has arrived for 1s. Consecutive duplicates are dropped, since the two
// file watchers batch bursts of file system events differently.
func collectEvents(events <-chan any) []any {
	collected := []any{}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev := <-events:
			ev = toAny(ev)
			normalize(ev, nil)
			if n := len(collected); n == 0 || !reflect.DeepEqual(collected[n-1], ev) {
				collected = append(collected, ev)
			}
			timer.Reset(time.Second)
		case <-timer.C:
			return collected
		}
	}
}

// outputStep records the warnings a server printed.
type outputStep struct{}

func (outputStep) label() string { return "output warnings" }

func (outputStep) run(t *testing.T, insts []*instance) []any {
	var results []any
	for _, inst := range insts {
		warnings := []any{}
		for _, line := range strings.Split(inst.out.String(), "\n") {
			if strings.Contains(line, "Warning") {
				warnings = append(warnings, strings.TrimRight(line, "\r"))
			}
		}
		results = append(results, warnings)
	}
	return results
}

// ghCallsStep records the arguments of every fakegh invocation.
type ghCallsStep struct{}

func (ghCallsStep) label() string { return "gh calls" }

func (ghCallsStep) run(t *testing.T, insts []*instance) []any {
	var results []any
	for _, inst := range insts {
		data, err := os.ReadFile(filepath.Join(inst.ghDir, "calls.jsonl"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		calls := []any{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var args any
			if err := json.Unmarshal([]byte(line), &args); err != nil {
				t.Fatal(err)
			}
			calls = append(calls, args)
		}
		results = append(results, calls)
	}
	return results
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// instance is one running server (Node or Go) with its own copy of the
// scenario's fixture repository.
type instance struct {
	name   string
	port   int
	out    *syncBuffer
	repo   *testrepo.Repo
	ghDir  string
	events chan any
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func buildBinary(t *testing.T, name, pkg string) string {
	bin := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

var startedPattern = regexp.MustCompile(`difit server started on http://[^\s:]+:(\d+)`)

func startInstance(t *testing.T, name string, argv []string, sc scenario, ghBin string) *instance {
	t.Helper()
	inst := &instance{name: name, out: &syncBuffer{}, repo: sc.fixture(t)}
	env := append(os.Environ(), "DIFIT_CONFIG_DIR="+t.TempDir(), "DIFIT_EDITOR=", "EDITOR=")
	if sc.gh != nil {
		inst.ghDir = t.TempDir()
		sc.gh(t, inst.repo, inst.ghDir)
		env = append(env, "FAKE_GH_DIR="+inst.ghDir, "PATH="+filepath.Dir(ghBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	args := append(append(argv[1:len(argv):len(argv)], sc.args...), "--no-open", "--keep-alive", "--port", strconv.Itoa(freePort(t)))
	cmd := exec.Command(argv[0], args...)
	cmd.Dir = inst.repo.Dir
	cmd.Env = env
	if sc.stdin != nil {
		cmd.Stdin = strings.NewReader(sc.stdin(inst.repo))
	}
	cmd.Stdout, cmd.Stderr = inst.out, inst.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
	})

	// Parallel scenarios can race for ports, so use the port the server
	// reports rather than the one requested.
	deadline := time.After(20 * time.Second)
	for inst.port == 0 {
		if m := startedPattern.FindStringSubmatch(inst.out.String()); m != nil {
			inst.port, _ = strconv.Atoi(m[1])
			break
		}
		select {
		case <-exited:
			t.Fatalf("%s server exited:\n%s", name, inst.out)
		case <-deadline:
			t.Fatalf("%s server did not start:\n%s", name, inst.out)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if sc.watch {
		inst.subscribe(t)
	}
	return inst
}

func (inst *instance) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(inst.port) + path
}

func (inst *instance) subscribe(t *testing.T) {
	resp, err := http.Get(inst.url("/api/watch"))
	if err != nil {
		t.Fatalf("%s watch: %v", inst.name, err)
	}
	inst.events = make(chan any, 256)
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			var ev any = data
			var decoded any
			if json.Unmarshal([]byte(data), &decoded) == nil {
				ev = decoded
			}
			select {
			case inst.events <- ev:
			default: // nobody is collecting; drop
			}
		}
	}()
}

func (inst *instance) do(t *testing.T, r request) any {
	t.Helper()
	req, err := http.NewRequest(r.method, inst.url(r.path), strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s %s: %v", inst.name, r.method, r.path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	result := map[string]any{"status": resp.StatusCode}
	if !r.statusOnly {
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		result["contentType"] = mediaType
		switch {
		case mediaType == "application/json":
			var v any
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatalf("%s %s: %v: %s", inst.name, r.path, err, data)
			}
			result["body"] = v
		case utf8.Valid(data):
			result["body"] = string(data)
		default:
			sum := sha256.Sum256(data)
			result["body"] = fmt.Sprintf("<%d bytes, sha256 %x>", len(data), sum)
		}
	}
	return inst.normalize(toAny(result))
}

// toAny round-trips v through JSON so live results compare equal to ones
// loaded from testdata.
func toAny(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

func (inst *instance) normalize(v any) any {
	dir := inst.repo.Dir
	r := &replacements{
		paths:  []string{dir, filepath.ToSlash(dir)},
		repoID: map[string]bool{sha256Hex(dir): true, sha256Hex(filepath.ToSlash(dir)): true},
	}
	return normalize(v, r)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type replacements struct {
	paths  []string
	repoID map[string]bool
}

var recentTimestamp = regexp.MustCompile(`^20\d\d-\d\d-\d\dT\d\d:\d\d:\d\d\.\d\d\dZ$`)

// normalize replaces values that legitimately differ between runs:
// timestamps generated by the server, and the fixture repository's
// location (which repositoryId hashes).
func normalize(v any, r *replacements) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok {
				if (k == "createdAt" || k == "updatedAt" || k == "timestamp") && recentTimestamp.MatchString(s) {
					if ts, err := time.Parse(time.RFC3339, s); err == nil && time.Since(ts) < time.Hour {
						x[k] = "<now>"
						continue
					}
				}
				if k == "repositoryId" && r != nil && r.repoID[s] {
					x[k] = "<repositoryId>"
					continue
				}
			}
			x[k] = normalize(val, r)
		}
	case []any:
		for i := range x {
			x[i] = normalize(x[i], r)
		}
	case string:
		if r != nil {
			for _, p := range r.paths {
				x = strings.ReplaceAll(x, p, "<repo>")
			}
		}
		return x
	}
	return v
}

func pretty(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

// firstDifference describes where two decoded JSON values diverge.
func firstDifference(path string, a, b any) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			break
		}
		for k := range x {
			if _, ok := y[k]; !ok {
				return fmt.Sprintf("%s.%s only in reference: %s", path, k, pretty(x[k]))
			}
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				return fmt.Sprintf("%s.%s only in Go: %s", path, k, pretty(y[k]))
			}
		}
		for k := range x {
			if d := firstDifference(path+"."+k, x[k], y[k]); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			break
		}
		for i := range x {
			if d := firstDifference(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if reflect.DeepEqual(a, b) {
		return ""
	}
	return fmt.Sprintf("%s:\n  reference: %s\n  go:        %s", path, pretty(a), pretty(b))
}

// buildFixture creates a repository exercising renames, deletions, binary
// files, generated files, unusual paths and branches.
func buildFixture(t *testing.T) *testrepo.Repo {
	r := testrepo.New(t)
	var app strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&app, "export const line%d = %d;\n", i, i)
	}
	r.Write("README.md", "# Fixture\n\nSome text.\n")
	r.Write("src/app.ts", app.String())
	r.Write("src/old-name.ts", "export const a = 1;\nexport const b = 2;\nexport const c = 3;\n")
	r.Write("deleted.txt", "goodbye\n")
	r.Write("whitespace.txt", "alpha beta\ngamma\n")
	r.Write("dir with space/ファイル.txt", "こんにちは\n")
	r.Write("assets/logo.png", "\x89PNG\r\n\x1a\n\x00\x00binary\x00")
	r.Write(".gitattributes", "gen/** linguist-generated=true\n")
	r.Write("gen/out.ts", "export {};\n")
	r.Write("package-lock.json", "{}\n")
	r.Commit("initial")

	r.Git("checkout", "-q", "-b", "feature")
	r.Write("feature.ts", "export const feature = true;\n")
	r.Commit("feature work")
	r.Git("checkout", "-q", "main")

	modified := strings.Replace(app.String(), "line3 = 3", "line3 = 300", 1)
	modified = strings.Replace(modified, "line30 = 30;\n", "line30 = 30;\nexport const inserted = true;\n", 1)
	r.Write("src/app.ts", modified)
	r.Git("mv", "src/old-name.ts", "src/new-name.ts")
	r.Write("src/new-name.ts", "export const a = 1;\nexport const b = 2;\nexport const c = 4;\n")
	r.Git("rm", "-q", "deleted.txt")
	r.Write("whitespace.txt", "alpha  beta\ngamma\n")
	r.Write("dir with space/ファイル.txt", "こんばんは\n")
	r.Write("assets/logo.png", "\x89PNG\r\n\x1a\n\x00\x00changed\x00")
	r.Write("gen/out.ts", "export const x = 1;\n")
	r.Write("package-lock.json", "{\"lockfileVersion\": 3}\n")
	r.Write("src/api.ts", "// @generated by protoc\nexport {};\n")
	r.Write("no-newline.txt", "no newline at end")
	r.Write("crlf.txt", "one\r\ntwo\r\n")
	r.Commit("main changes")
	r.Git("branch", "Zeta")
	r.Git("branch", "alpha")

	r.Write("README.md", "# Fixture\n\nSome staged text.\n")
	r.Git("add", "README.md")
	r.Write("src/app.ts", strings.Replace(modified, "line10 = 10", "line10 = 1000", 1))
	return r
}

func commonRequests() []step {
	return []step{
		get("/api/diff"),
		get("/api/diff?ignoreWhitespace=true"),
		get("/api/revisions"),
		get("/api/generated-status/package-lock.json"),
		get("/api/generated-status/gen/out.ts"),
		get("/api/generated-status/src/api.ts?ref=HEAD"),
		get("/api/generated-status/src/app.ts"),
		get("/api/generated-status/..%2Fsecret"),
		get("/api/line-count/src/app.ts?oldRef=HEAD~1&newRef=HEAD"),
		get("/api/line-count/src/new-name.ts?oldRef=HEAD~1&newRef=HEAD&oldPath=src/old-name.ts"),
		get("/api/line-count/no-newline.txt?newRef=working"),
		get("/api/line-count/missing.txt?oldRef=HEAD"),
		get("/api/line-count/src/app.ts?oldPath=..%2Fx&oldRef=HEAD"),
		get("/api/blob/assets/logo.png?ref=HEAD~1"),
		get("/api/blob/assets%2Flogo.png"),
		get("/api/blob/src/app.ts?ref=working"),
		get("/api/blob/README.md?ref=staged"),
		get("/api/blob/dir%20with%20space%2F%E3%83%95%E3%82%A1%E3%82%A4%E3%83%AB.txt?ref=HEAD"),
		get("/api/blob/missing.png"),
		get("/api/blob/%2Fetc%2Fpasswd"),
		get("/api/comments-json"),
		get("/api/comments-output"),
	}
}

func commentRequests() []step {
	thread := `{"id":"t1","filePath":"src/app.ts","createdAt":"2024-01-01T00:00:00.000Z","updatedAt":"2024-01-01T00:00:00.000Z",` +
		`"position":{"side":"new","line":{"start":3,"end":4}},"codeSnapshot":{"content":"x","language":"typescript"},` +
		`"messages":[{"id":"t1","body":"Rename?\n` + "```suggestion\\nconst y = 1;\\n```" + `","createdAt":"2024-01-01T00:00:00.000Z","updatedAt":"2024-01-01T00:00:00.000Z"}]}`
	imports := `[{"type":"thread","id":"agent-1","filePath":"README.md","position":{"side":"old","line":2},"body":"From agent","author":"Agent",` +
		`"createdAt":"2024-02-01T00:00:00.000Z","updatedAt":"2024-02-01T00:00:00.000Z"},` +
		`{"type":"reply","id":"agent-2","filePath":"README.md","position":{"side":"old","line":2},"body":"Reply","author":"Agent","createdAt":"2024-02-02T00:00:00.000Z"}]`
	return []step{
		post("/api/comments", `{"threads":[`+thread+`]}`),
		post("/api/comment-imports", imports),
		post("/api/comment-imports", imports),
		get("/api/comments-json"),
		post("/api/comments", `{"baseVersion":1,"threads":[`+thread+`]}`),
		post("/api/comments", `{"comments":[{"id":"c1","file":"a.ts","line":[5,6],"body":"Legacy","timestamp":"2024-03-01T00:00:00.000Z","codeContent":"z"},`+
			`{"file":"b.ts","line":7,"body":"No id","timestamp":"2024-03-01T00:00:00.000Z"}]}`),
		post("/api/comments", `{"threads":[{"id":"f1","file":"c.ts","line":9,"side":"old","createdAt":"2024-04-01T00:00:00.000Z",`+
			`"messages":[{"body":"Flat","author":"Me","createdAt":"2024-04-01T00:00:00.000Z"},{"id":"m2","body":"Second","createdAt":"2024-04-02T00:00:00.000Z"}]}]}`),
		get("/api/comments-json"),
		get("/api/comments-output"),
		request{method: "DELETE", path: "/api/comments/f1"},
		request{method: "DELETE", path: "/api/comments/f1"},
		// Express answers malformed JSON with an HTML stack trace.
		request{method: "POST", path: "/api/comments", body: `{not json`, statusOnly: true},
		post("/api/comment-imports", `[{"type":"nope"}]`),
		post("/api/comment-imports", `{"type":"thread","filePath":"x","position":{"side":"new","line":0},"body":"b"}`),
		get("/api/user-settings"),
		request{method: "PUT", path: "/api/user-settings", body: `{"client":{"theme":"dark","fontSize":14}}`},
		request{method: "PUT", path: "/api/user-settings", body: `{"client":{"theme":"light"}}`},
		request{method: "PUT", path: "/api/user-settings", body: `{"client":[]}`},
		get("/api/user-settings"),
		post("/api/open-in-editor", `{"filePath":"src/app.ts","line":3,"editor":{"id":"none"}}`),
		post("/api/open-in-editor", `{"filePath":"src/app.ts","editor":{"id":"custom","command":"","argsTemplate":""}}`),
		post("/api/open-in-editor", `{"filePath":"src/app.ts","editor":{"id":"zed","command":"definitely-not-installed-xyz","argsTemplate":"%file"}}`),
		post("/api/open-in-editor", `{"filePath":"../outside"}`),
		post("/api/open-in-editor", `{"line":3}`),
	}
}

// buildWatchFixture creates a small repository for the watch scenarios,
// which modify it.
func buildWatchFixture(t *testing.T) *testrepo.Repo {
	r := testrepo.New(t)
	r.Write(".gitignore", "ignored/\n")
	r.Write("a.txt", "one\n")
	r.Commit("initial")
	r.Write("a.txt", "one\ntwo\n")
	r.Commit("second")
	return r
}

func watchSteps() []step {
	thread := `{"threads":[{"id":"w1","filePath":"a.txt","createdAt":"2024-01-01T00:00:00.000Z","updatedAt":"2024-01-01T00:00:00.000Z",` +
		`"position":{"side":"new","line":2},"messages":[{"id":"w1","body":"Watch me","createdAt":"2024-01-01T00:00:00.000Z","updatedAt":"2024-01-01T00:00:00.000Z"}]}]}`
	return []step{
		watchStep{name: "connect"},
		post("/api/comments", thread),
		watchStep{name: "comments saved"},
		watchStep{name: "edit tracked file", action: func(r *testrepo.Repo) { r.Write("a.txt", "one\ntwo\nthree\n") }},
		get("/api/diff"),
		watchStep{name: "write gitignored file", action: func(r *testrepo.Repo) { r.Write("ignored/x.txt", "x\n") }},
		watchStep{name: "write file in node_modules", action: func(r *testrepo.Repo) { r.Write("node_modules/m/index.js", "x\n") }},
		watchStep{name: "stage file", action: func(r *testrepo.Repo) { r.Git("add", "a.txt") }},
		get("/api/diff"),
		watchStep{name: "commit", action: func(r *testrepo.Repo) { r.Commit("third") }},
		get("/api/diff"),
		watchStep{name: "create directory", action: func(r *testrepo.Repo) { r.Write("newdir/b.txt", "b\n") }},
		watchStep{name: "edit file in new directory", action: func(r *testrepo.Repo) { r.Write("newdir/b.txt", "b2\n") }},
		get("/api/diff"),
	}
}

const prURL = "https://github.com/owner/repo/pull/123"

// prReviewThreads is the fake GraphQL review-thread data, as two pages.
var prReviewThreads = []string{
	`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
		{"id":"T1","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","startDiffSide":null,"line":3,"startLine":null,"originalLine":3,"originalStartLine":null,
		 "comments":{"nodes":[
			{"id":"C2","body":"Agreed","createdAt":"2024-05-02T00:00:00Z","updatedAt":"2024-05-02T00:00:00Z","author":{"login":"bob"}},
			{"id":"C1","body":"Why 300?","createdAt":"2024-05-01T00:00:00Z","updatedAt":"2024-05-01T01:00:00Z","author":{"login":"alice"}}]}},
		{"id":"T2","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"LEFT","startDiffSide":"LEFT","line":null,"startLine":null,"originalLine":4,"originalStartLine":2,
		 "comments":{"nodes":[{"id":"C3","body":"Old range","createdAt":"2024-05-03T00:00:00Z","updatedAt":"2024-05-03T00:00:00Z","author":null}]}},
		{"id":"T3","isResolved":true,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","line":5,
		 "comments":{"nodes":[{"id":"C4","body":"Resolved","createdAt":"2024-05-04T00:00:00Z","updatedAt":"2024-05-04T00:00:00Z"}]}},
		{"id":"T4","isResolved":false,"isOutdated":true,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","line":6,
		 "comments":{"nodes":[{"id":"C5","body":"Outdated","createdAt":"2024-05-04T00:00:00Z","updatedAt":"2024-05-04T00:00:00Z"}]}},
		{"id":"T5","isResolved":false,"isOutdated":false,"subjectType":"FILE","path":"src/app.ts","diffSide":"RIGHT",
		 "comments":{"nodes":[{"id":"C6","body":"File level","createdAt":"2024-05-04T00:00:00Z","updatedAt":"2024-05-04T00:00:00Z"}]}},
		{"id":"T6","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","line":null,
		 "comments":{"nodes":[{"id":"C7","body":"No line","createdAt":"2024-05-04T00:00:00Z","updatedAt":"2024-05-04T00:00:00Z"}]}},
		{"id":"T7","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":" ","diffSide":"RIGHT","line":2,
		 "comments":{"nodes":[{"id":"C8","body":"No path","createdAt":"2024-05-04T00:00:00Z","updatedAt":"2024-05-04T00:00:00Z"}]}},
		{"id":"T8","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"README.md","diffSide":"RIGHT","line":2,
		 "comments":{"nodes":[{"id":"C9","body":"Missing updatedAt","createdAt":"2024-05-04T00:00:00Z"}]}},
		{"id":"T9","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/new-name.ts","diffSide":"RIGHT","startDiffSide":null,"line":3,"startLine":3,
		 "comments":{"nodes":[
			{"id":"C10","body":"Redundant start","createdAt":"2024-05-05T00:00:00Z","updatedAt":"2024-05-05T00:00:00Z","author":{"login":"carol"}},
			{"id":"C11","body":"Broken reply","createdAt":"2024-05-06T00:00:00Z"}]}},
		{"id":"T10","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","line":7,"comments":{"nodes":[]}}
	],"pageInfo":{"hasNextPage":true,"endCursor":"c2"}}}}}}`,
	`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
		{"id":"T11","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","startDiffSide":"LEFT","line":9,"startLine":8,
		 "comments":{"nodes":[{"id":"C12","body":"Mismatched sides","createdAt":"2024-05-07T00:00:00Z","updatedAt":"2024-05-07T00:00:00Z"}]}},
		{"id":"T12","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"RIGHT","startDiffSide":"RIGHT","line":31,"startLine":29,
		 "comments":{"nodes":[{"id":"C13","body":"Range on page two","createdAt":"2024-05-08T00:00:00Z","updatedAt":"2024-05-08T00:00:00Z","author":{"login":"dave"}}]}},
		{"id":"T13","isResolved":false,"isOutdated":false,"subjectType":"LINE","path":"src/app.ts","diffSide":"BOTH","line":1,
		 "comments":{"nodes":[{"id":"C14","body":"Odd side","createdAt":"2024-05-09T00:00:00Z","updatedAt":"2024-05-09T00:00:00Z"}]}}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`,
}

// fakeGitHub serves the fixture's last commit as the PR patch, and either
// the review threads above or a failing GraphQL call.
func fakeGitHub(graphqlError string) func(t *testing.T, r *testrepo.Repo, dir string) {
	return func(t *testing.T, r *testrepo.Repo, dir string) {
		files := map[string]string{"pr.patch": r.Git("diff", "HEAD~1", "HEAD") + "\n"}
		if graphqlError != "" {
			files["graphql-1.json.err"] = graphqlError
		} else {
			files["graphql-1.json"] = prReviewThreads[0]
			files["graphql-c2.json"] = prReviewThreads[1]
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type scenario struct {
	name    string
	fixture func(t *testing.T) *testrepo.Repo
	args    []string
	stdin   func(r *testrepo.Repo) string
	gh      func(t *testing.T, r *testrepo.Repo, dir string)
	watch   bool
	steps   []step
}

func scenarios() []scenario {
	stdinDiff := func(r *testrepo.Repo) string { return r.Git("diff", "HEAD~1", "HEAD") + "\n" }
	stdinImports := `[{"type":"thread","id":"s1","filePath":"src/app.ts","position":{"side":"new","line":3},"body":"On stdin","createdAt":"2024-01-01T00:00:00.000Z","updatedAt":"2024-01-01T00:00:00.000Z"}]`

	list := []scenario{
		{name: "default", steps: append(append(commonRequests(), commentRequests()...),
			get("/api/diff?base=main&target=feature&baseMode=merge-base"),
			get("/api/revisions"),
			get("/api/diff?base=HEAD~1&target=HEAD"),
			get("/api/diff?base=Zeta&target=alpha"),
			// simple-git words git failures differently.
			request{method: "GET", path: "/api/diff?base=HEAD&target=nope", statusOnly: true},
			get("/api/diff?base=working&target=HEAD"),
			get("/api/diff?base=staged&target=working"),
			get("/api/diff?base=HEAD&target=staged"),
			get("/api/diff?base=HEAD&target=."),
			get("/api/generated-status/src/app.ts"),
			get("/api/comments-output"),
		)},
		{name: "working", args: []string{"working"}, steps: commonRequests()},
		{name: "staged", args: []string{"staged"}, steps: commonRequests()},
		{name: "dot", args: []string{"."}, steps: commonRequests()},
		{name: "compare", args: []string{"feature", "main", "--context", "1"}, steps: commonRequests()},
		{name: "merge-base", args: []string{"feature", "main", "--merge-base"}, steps: commonRequests()},
		{name: "clean with imports", args: []string{"HEAD", "--clean", "--comment", stdinImports}, steps: []step{
			get("/api/diff"),
			get("/api/comments-json"),
			get("/api/diff?base=HEAD~1&target=HEAD"),
			get("/api/diff?base=feature&target=HEAD"),
			get("/api/comments-json"),
		}},
		{name: "stdin", args: []string{"-", "--comment", stdinImports}, stdin: stdinDiff, steps: []step{
			get("/api/diff"),
			get("/api/diff?base=other&target=thing"),
			get("/api/revisions"),
			get("/api/generated-status/src/app.ts"),
			get("/api/line-count/src/app.ts?newRef=HEAD"),
			get("/api/blob/src/app.ts"),
			get("/api/comments-json"),
			get("/api/comments-output"),
			post("/api/open-in-editor", `{"filePath":"src/app.ts"}`),
		}},
		{name: "pr", args: []string{"--pr", prURL}, gh: fakeGitHub(""), steps: []step{
			get("/api/diff"),
			get("/api/revisions"),
			get("/api/comments-json"),
			get("/api/comments-output"),
			outputStep{},
			ghCallsStep{},
		}},
		{name: "pr review threads failing", args: []string{"--pr", prURL}, gh: fakeGitHub("HTTP 401: Bad credentials (https://api.github.com/graphql)\n"), steps: []step{
			get("/api/comments-json"),
			outputStep{},
			ghCallsStep{},
		}},
	}
	for i := range list {
		list[i].fixture = buildFixture
	}

	for _, mode := range []struct {
		name string
		args []string
	}{
		{"default", nil},
		{"working", []string{"working"}},
		{"staged", []string{"staged"}},
		{"dot", []string{"."}},
		{"specific", []string{"HEAD~1", "HEAD"}},
	} {
		list = append(list, scenario{
			name:    "watch " + mode.name,
			fixture: buildWatchFixture,
			args:    mode.args,
			watch:   true,
			steps:   watchSteps(),
		})
	}
	return list
}

type observation struct {
	Step   string `json:"step"`
	Result any    `json:"result"`
}

func goldenPath(sc scenario) string {
	return filepath.Join("testdata", strings.ReplaceAll(sc.name, " ", "-")+".json")
}

func readGolden(t *testing.T, path string) []observation {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (record it with DIFIT_REFERENCE=... go test ./internal/conformance -update)", err)
	}
	var obs []observation
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return obs
}

func writeGolden(t *testing.T, path string, obs []observation) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obs); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runScenario runs the steps against the Go binary and compares each result
// with the reference server's (when ref is set) or with the recording.
func runScenario(t *testing.T, sc scenario, ref []string, goBin, ghBin string) {
	var insts []*instance
	if ref != nil {
		insts = append(insts, startInstance(t, "reference", ref, sc, ghBin))
	}
	insts = append(insts, startInstance(t, "go", []string{goBin}, sc, ghBin))

	path := goldenPath(sc)
	var recorded []observation
	if ref == nil {
		recorded = readGolden(t, path)
		if len(recorded) != len(sc.steps) {
			t.Fatalf("%s has %d steps, the scenario %d; re-record it", path, len(recorded), len(sc.steps))
		}
	}
	for i, st := range sc.steps {
		results := st.run(t, insts)
		if ref != nil {
			recorded = append(recorded, observation{Step: st.label(), Result: results[0]})
		} else if recorded[i].Step != st.label() {
			t.Fatalf("%s step %d is %q, the scenario's %q; re-record it", path, i, recorded[i].Step, st.label())
		}
		if d := firstDifference("$", recorded[i].Result, results[len(results)-1]); d != "" {
			t.Errorf("step %d, %s: %s", i, st.label(), d)
		}
	}
	if *update {
		writeGolden(t, path, recorded)
	}
}

func TestConformance(t *testing.T) {
	var ref []string
	if script := os.Getenv("DIFIT_REFERENCE"); script != "" {
		ref = []string{"node", script}
	} else if *update {
		t.Fatal("-update needs DIFIT_REFERENCE")
	}
	goBin := buildBinary(t, "difit", "github.com/tdryer/difit-go/cmd/difit")
	ghBin := buildBinary(t, "gh", "github.com/tdryer/difit-go/internal/conformance/fakegh")
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc, ref, goBin, ghBin)
		})
	}
}
