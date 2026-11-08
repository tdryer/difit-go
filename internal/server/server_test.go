package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tdryer/difit-go/internal/testrepo"
	"github.com/tdryer/difit-go/internal/types"
)

type harness struct {
	t      *testing.T
	repo   *testrepo.Repo
	server *Server
	http   *httptest.Server
	stdout *syncBuffer
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

func newRepo(t *testing.T) *testrepo.Repo {
	r := testrepo.New(t)
	r.Write("src/a.ts", "one\ntwo\n")
	r.Write("img/logo.png", "PNGDATA")
	r.Commit("first")
	r.Write("src/a.ts", "one\nTWO\nthree\n")
	r.Write("yarn.lock", "lock\n")
	r.Commit("second")
	return r
}

func start(t *testing.T, opts Options) *harness {
	t.Helper()
	h := &harness{t: t, stdout: &syncBuffer{}}
	if opts.RepoPath == "" && opts.StdinDiff == "" {
		h.repo = newRepo(t)
		opts.RepoPath = h.repo.Dir
	}
	if opts.Selection == (types.DiffSelection{}) && opts.StdinDiff == "" {
		opts.Selection = types.NewDiffSelection("HEAD^", "HEAD", "")
	}
	if opts.ContextLines == 0 {
		opts.ContextLines = -1
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	}
	opts.Stdout, opts.Stderr = h.stdout, io.Discard
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.server = s
	h.http = httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		h.http.Close()
		s.Close()
	})
	return h
}

func (h *harness) do(method, path, contentType, body string) (int, http.Header, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.http.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func (h *harness) json(method, path, body string, wantStatus int) map[string]any {
	h.t.Helper()
	status, _, data := h.do(method, path, "application/json", body)
	if status != wantStatus {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		h.t.Fatalf("%s %s: %v: %s", method, path, err, data)
	}
	return v
}

func (h *harness) text(path string) string {
	h.t.Helper()
	_, _, data := h.do("GET", path, "", "")
	return string(data)
}

func TestDiffEndpoint(t *testing.T) {
	h := start(t, Options{ClearComments: true})
	resp := h.json("GET", "/api/diff", "", 200)

	head := h.repo.Git("rev-parse", "--short=7", "HEAD")
	parent := h.repo.Git("rev-parse", "--short=7", "HEAD^")
	want := map[string]any{
		"commit":                   parent + "..." + head,
		"baseCommitish":            parent,
		"targetCommitish":          head,
		"requestedBaseCommitish":   "HEAD^",
		"requestedTargetCommitish": "HEAD",
		"ignoreWhitespace":         false,
		"openInEditorAvailable":    true,
		"clearComments":            true,
		"isEmpty":                  false,
		"repositoryId":             sha256Hex(h.repo.Dir),
	}
	for k, v := range want {
		if resp[k] != v {
			t.Errorf("%s = %v, want %v", k, resp[k], v)
		}
	}
	for _, k := range []string{"requestedBaseMode", "commentImports", "commentImportId"} {
		if _, ok := resp[k]; ok {
			t.Errorf("unexpected %s", k)
		}
	}
	if files := resp["files"].([]any); len(files) != 2 {
		t.Fatalf("files %v", files)
	}

	resp = h.json("GET", "/api/diff?ignoreWhitespace=true&base=HEAD~1&target=HEAD&baseMode=merge-base", "", 200)
	if resp["ignoreWhitespace"] != true || resp["requestedBaseMode"] != "merge-base" || resp["requestedBaseCommitish"] != "HEAD~1" {
		t.Fatalf("query selection: %v", resp)
	}
	// Without parameters the previous selection, including its mode, is kept.
	resp = h.json("GET", "/api/diff", "", 200)
	if resp["requestedBaseMode"] != "merge-base" {
		t.Fatalf("selection not kept: %v", resp["requestedBaseMode"])
	}
	// Naming a base without baseMode resets to direct.
	resp = h.json("GET", "/api/diff?base=HEAD~1", "", 200)
	if _, ok := resp["requestedBaseMode"]; ok {
		t.Fatalf("baseMode not reset")
	}
}

func TestDiffErrorDoesNotPoisonLaterRequests(t *testing.T) {
	h := start(t, Options{})
	resp := h.json("GET", "/api/diff?base=HEAD&target=nope", "", 500)
	if !strings.HasPrefix(resp["error"].(string), "Failed to parse diff for nope vs HEAD: ") {
		t.Fatalf("error %v", resp)
	}
	h.json("GET", "/api/diff?base=HEAD^&target=HEAD", "", 200)
}

func TestDiffCacheIsLRU(t *testing.T) {
	h := start(t, Options{})
	for i := 0; i < 10; i++ {
		h.json("GET", "/api/diff?base=HEAD^&target=HEAD&ignoreWhitespace="+strconv.FormatBool(i%2 == 0)+"&baseMode=x"+strconv.Itoa(i), "", 200)
	}
	h.json("GET", "/api/diff?base=HEAD~1&target=HEAD", "", 200)
	for i := 0; i < maxDiffCacheEntries+2; i++ {
		h.json("GET", "/api/diff?base=HEAD^&target="+strings.Repeat("HEAD~0", 0)+"HEAD&ignoreWhitespace=true&x="+strconv.Itoa(i), "", 200)
	}
	h.server.mu.Lock()
	n := len(h.server.diffCache)
	h.server.mu.Unlock()
	if n > maxDiffCacheEntries {
		t.Fatalf("cache has %d entries", n)
	}

	h.server.mu.Lock()
	h.server.diffCache = nil
	for i := 0; i < maxDiffCacheEntries; i++ {
		h.server.setCachedDiffLocked("k"+strconv.Itoa(i), types.DiffResponse{})
	}
	h.server.getCachedDiffLocked("k0")
	h.server.setCachedDiffLocked("new", types.DiffResponse{})
	var keys []string
	for _, e := range h.server.diffCache {
		keys = append(keys, e.key)
	}
	h.server.mu.Unlock()
	if slices.Contains(keys, "k1") || !slices.Contains(keys, "k0") || keys[len(keys)-1] != "new" {
		t.Fatalf("eviction order: %v", keys)
	}
}

func TestDiffCommentImports(t *testing.T) {
	imports := []types.CommentImport{{
		Type:     "thread",
		FilePath: "src/a.ts",
		Position: types.DiffCommentPosition{Side: "new", Line: types.SingleLine(2)},
		Body:     "Startup comment",
	}}
	h := start(t, Options{CommentImports: imports, ClearComments: true})
	resp := h.json("GET", "/api/diff", "", 200)
	if resp["commentImportId"] == nil || len(resp["commentImports"].([]any)) != 1 || resp["clearComments"] != true {
		t.Fatalf("imports missing: %v", resp)
	}
	resp = h.json("GET", "/api/diff?base=HEAD~1&target=HEAD", "", 200)
	if _, ok := resp["commentImports"]; ok {
		t.Fatal("imports included for another selection")
	}
	// Comment sessions are keyed by resolved hashes, so go back to the
	// initial selection before reading them.
	h.json("GET", "/api/diff?base=HEAD%5E&target=HEAD", "", 200)
	if !strings.Contains(h.text("/api/comments-output"), "Startup comment") {
		t.Fatal("startup import not in session")
	}
}

func TestGeneratedStatus(t *testing.T) {
	h := start(t, Options{})
	resp := h.json("GET", "/api/generated-status/yarn.lock", "", 200)
	if resp["path"] != "yarn.lock" || resp["ref"] != "HEAD" || resp["isGenerated"] != true || resp["source"] != "path" {
		t.Fatalf("%v", resp)
	}
	resp = h.json("GET", "/api/generated-status/src/a.ts?ref=HEAD^", "", 200)
	if resp["ref"] != "HEAD^" || resp["isGenerated"] != false {
		t.Fatalf("%v", resp)
	}
	resp = h.json("GET", "/api/generated-status/%2Fetc%2Fpasswd", "", 400)
	if resp["error"] != "File path outside repository" {
		t.Fatalf("%v", resp)
	}
	resp = h.json("GET", "/api/generated-status/src%2F..%2F..%2Fx", "", 400)
	if resp["error"] != "File path outside repository" {
		t.Fatalf("%v", resp)
	}
}

func TestRevisions(t *testing.T) {
	h := start(t, Options{})
	resp := h.json("GET", "/api/revisions", "", 200)
	special := resp["specialOptions"].([]any)
	if len(special) != 3 || special[0].(map[string]any)["label"] != "All Uncommitted Changes" {
		t.Fatalf("%v", special)
	}
	if len(resp["branches"].([]any)) != 1 || len(resp["commits"].([]any)) != 2 {
		t.Fatalf("%v", resp)
	}
	if resp["resolvedBase"] != h.repo.Git("rev-parse", "--short=7", "HEAD^") {
		t.Fatalf("resolvedBase %v", resp["resolvedBase"])
	}
}

func TestLineCount(t *testing.T) {
	h := start(t, Options{})
	resp := h.json("GET", "/api/line-count/src/a.ts?oldRef=HEAD^&newRef=HEAD", "", 200)
	if resp["oldLineCount"] != 2.0 || resp["newLineCount"] != 3.0 {
		t.Fatalf("%v", resp)
	}
	resp = h.json("GET", "/api/line-count/missing.ts?newRef=HEAD", "", 200)
	if resp["newLineCount"] != 0.0 || resp["oldLineCount"] != nil {
		t.Fatalf("%v", resp)
	}
	h.json("GET", "/api/line-count/src/a.ts?oldPath=..%2Fx&oldRef=HEAD", "", 400)
}

func TestBlob(t *testing.T) {
	h := start(t, Options{})
	status, header, body := h.do("GET", "/api/blob/img/logo.png?ref=HEAD", "", "")
	if status != 200 || string(body) != "PNGDATA" || header.Get("Content-Type") != "image/png" ||
		header.Get("Cache-Control") != "no-cache, no-store, must-revalidate" {
		t.Fatalf("%d %v %q", status, header, body)
	}
	_, header, body = h.do("GET", "/api/blob/src/a.ts", "", "")
	if string(body) != "one\nTWO\nthree\n" || header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("default ref: %q %s", body, header.Get("Content-Type"))
	}
	_, _, body = h.do("GET", "/api/blob/src%2Fa.ts?ref=HEAD%5E", "", "")
	if string(body) != "one\ntwo\n" {
		t.Fatalf("encoded path and ref: %q", body)
	}
	if resp := h.json("GET", "/api/blob/missing.png", "", 404); resp["error"] != "File not found" {
		t.Fatalf("%v", resp)
	}
	h.json("GET", "/api/blob/..%2F..%2Fetc%2Fpasswd", "", 400)

	h.repo.Write("dir with space/file #1.svg", "<svg/>")
	_, header, body = h.do("GET", "/api/blob/dir%20with%20space%2Ffile%20%231.svg?ref=working", "", "")
	if string(body) != "<svg/>" || header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("special characters: %q %s", body, header.Get("Content-Type"))
	}
}

func TestBlobContentType(t *testing.T) {
	cases := map[string]string{
		"a.JPG": "image/jpeg", "a.webp": "image/webp", "a.ico": "image/x-icon", "a.tif": "image/tiff",
		"a.heic": "image/heic", "a.txt": "application/octet-stream", "Makefile": "application/octet-stream",
	}
	for path, want := range cases {
		if got := blobContentType(path); got != want {
			t.Errorf("%s: %s", path, got)
		}
	}
}

const legacyComments = `{"comments":[
	{"id":"1","file":"src/App.tsx","line":10,"body":"Test comment","timestamp":"2024-01-01T00:00:00Z"},
	{"id":"2","file":"src/utils/helper.ts","line":[20,30],"body":"Another comment","timestamp":"2024-01-01T00:01:00Z"},
	{"line":5,"body":"Comment without file"}]}`

func TestPostLegacyComments(t *testing.T) {
	h := start(t, Options{})
	resp := h.json("POST", "/api/comments", legacyComments, 200)
	if resp["success"] != true || resp["merged"] != false || resp["version"] != 1.0 {
		t.Fatalf("%v", resp)
	}
	output := h.text("/api/comments-output")
	for _, want := range []string{"src/App.tsx:L10\nTest comment", "src/utils/helper.ts:L20-L30\nAnother comment", "<unknown file>:L5", "Total comments: 3"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "undefined") {
		t.Error("output contains undefined")
	}
}

func TestPostCommentsTextPlain(t *testing.T) {
	h := start(t, Options{})
	status, _, _ := h.do("POST", "/api/comments", "text/plain;charset=UTF-8", legacyComments)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	session := h.json("GET", "/api/comments-json", "", 200)
	if len(session["threads"].([]any)) != 3 {
		t.Fatalf("%v", session)
	}
}

func TestPostCommentsRejectsMalformed(t *testing.T) {
	h := start(t, Options{})
	for _, body := range []string{"{not json", "null", ""} {
		resp := h.json("POST", "/api/comments", body, 400)
		if resp["error"] != "Invalid comment data" {
			t.Fatalf("%q: %v", body, resp)
		}
	}
}

func TestPostFlattenedThreads(t *testing.T) {
	h := start(t, Options{})
	h.json("POST", "/api/comments", `{"threads":[{"file":"a.ts","line":[3,1],"side":"old","codeContent":"x",
		"createdAt":"2024-01-01T00:00:00.000Z","messages":[{"body":"hi","author":"Me"},{"id":"m2","body":"reply"}]}]}`, 200)
	session := h.json("GET", "/api/comments-json", "", 200)
	thread := session["threads"].([]any)[0].(map[string]any)
	messages := thread["messages"].([]any)
	id := thread["id"].(string)
	if len(id) != 12 || messages[0].(map[string]any)["id"] != id+":0" || messages[1].(map[string]any)["id"] != "m2" {
		t.Fatalf("ids: %v", thread)
	}
	position := thread["position"].(map[string]any)
	if position["side"] != "old" || position["line"] != 1.0 || thread["codeSnapshot"].(map[string]any)["content"] != "x" {
		t.Fatalf("position: %v", thread)
	}
	if messages[0].(map[string]any)["createdAt"] != "2024-01-01T00:00:00.000Z" || messages[0].(map[string]any)["author"] != "Me" {
		t.Fatalf("messages: %v", messages)
	}
}

func makeThread(id, file string, line int, body string) string {
	return `{"id":"` + id + `","filePath":"` + file + `","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z",
		"position":{"side":"new","line":` + strconv.Itoa(line) + `},
		"messages":[{"id":"` + id + `","body":"` + body + `","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z"}]}`
}

const agentImport = `[{"type":"thread","id":"agent-1","filePath":"src/b.ts","position":{"side":"new","line":20},"body":"agent finding","author":"Agent"}]`

func TestStalePushMerges(t *testing.T) {
	h := start(t, Options{})
	h.json("POST", "/api/comments", `{"threads":[`+makeThread("t1", "src/a.ts", 10, "human")+`]}`, 200)
	if v := h.json("GET", "/api/comments-json", "", 200)["version"]; v != 1.0 {
		t.Fatalf("version %v", v)
	}
	imp := h.json("POST", "/api/comment-imports", agentImport, 200)
	if imp["changed"] != true || imp["count"] != 1.0 || len(imp["importId"].(string)) != 64 || len(imp["warnings"].([]any)) != 0 {
		t.Fatalf("import: %v", imp)
	}

	resp := h.json("POST", "/api/comments", `{"baseVersion":1,"threads":[`+makeThread("t1", "src/a.ts", 10, "human")+`]}`, 200)
	if resp["merged"] != true || len(resp["threads"].([]any)) != 2 {
		t.Fatalf("stale push: %v", resp)
	}

	// A matching version replaces, honoring deletions.
	version := h.json("GET", "/api/comments-json", "", 200)["version"].(float64)
	resp = h.json("POST", "/api/comments", `{"threads":[],"baseVersion":`+strconv.Itoa(int(version))+`}`, 200)
	if resp["merged"] != false || len(resp["threads"].([]any)) != 0 {
		t.Fatalf("replace: %v", resp)
	}
}

func TestImportReplyBumpsVersion(t *testing.T) {
	h := start(t, Options{})
	h.json("POST", "/api/comments", `{"threads":[`+makeThread("t1", "src/a.ts", 10, "human")+`]}`, 200)
	reply := `[{"type":"reply","filePath":"src/a.ts","position":{"side":"new","line":10},"body":"agent reply","author":"Agent"}]`
	h.json("POST", "/api/comment-imports", reply, 200)
	if v := h.json("GET", "/api/comments-json", "", 200)["version"]; v != 2.0 {
		t.Fatalf("version %v", v)
	}
	// Importing the same reply again changes nothing.
	if resp := h.json("POST", "/api/comment-imports", reply, 200); resp["changed"] != false {
		t.Fatalf("%v", resp)
	}
	single := `{"type":"thread","filePath":"x.ts","position":{"side":"old","line":{"start":1,"end":2}},"body":"b"}`
	if resp := h.json("POST", "/api/comment-imports", single, 200); resp["count"] != 1.0 {
		t.Fatalf("single object: %v", resp)
	}
	if resp := h.json("POST", "/api/comment-imports", `[{"type":"bogus"}]`, 400); resp["error"] != "Invalid comment import data" {
		t.Fatalf("%v", resp)
	}
}

func TestDeleteComment(t *testing.T) {
	h := start(t, Options{})
	h.json("POST", "/api/comments", `{"threads":[`+makeThread("t1", "a", 1, "x")+`,`+makeThread("t2", "b", 2, "y")+`]}`, 200)
	resp := h.json("DELETE", "/api/comments/t1", "", 200)
	if resp["success"] != true || resp["threadId"] != "t1" || resp["version"] != 2.0 {
		t.Fatalf("%v", resp)
	}
	resp = h.json("DELETE", "/api/comments/t1", "", 404)
	if resp["error"] != "Thread not found: t1" {
		t.Fatalf("%v", resp)
	}
}

func TestCommentSessionsAreIsolatedPerSelection(t *testing.T) {
	imports := []types.CommentImport{{
		Type:     "thread",
		FilePath: "src/a.ts",
		Position: types.DiffCommentPosition{Side: "new", Line: types.SingleLine(10)},
		Body:     "Startup comment",
	}}
	h := start(t, Options{CommentImports: imports})
	h.repo.Git("checkout", "-q", "-b", "feature")
	h.repo.Write("f.txt", "f\n")
	h.repo.Commit("feature")
	h.repo.Git("checkout", "-q", "main")
	thread := func(line int, body string) string {
		return `[{"type":"thread","filePath":"src/a.ts","position":{"side":"new","line":` + strconv.Itoa(line) + `},"body":"` + body + `"}]`
	}
	h.json("POST", "/api/comment-imports", thread(20, "API comment"), 200)
	output := h.text("/api/comments-output")
	if !strings.Contains(output, "Startup comment") || !strings.Contains(output, "API comment") {
		t.Fatalf("initial session:\n%s", output)
	}

	h.json("GET", "/api/diff?base=HEAD~1&target=feature", "", 200)
	if output := h.text("/api/comments-output"); output != "" {
		t.Fatalf("other selection not empty:\n%s", output)
	}
	h.json("POST", "/api/comment-imports", thread(30, "Other diff comment"), 200)

	h.json("GET", "/api/diff?base=HEAD%5E&target=HEAD", "", 200)
	output = h.text("/api/comments-output")
	if !strings.Contains(output, "API comment") || strings.Contains(output, "Other diff comment") {
		t.Fatalf("back to initial selection:\n%s", output)
	}
	if !strings.Contains(h.server.FinalComments(), "Startup comment") {
		t.Fatal("final comments")
	}
}

func TestUserSettings(t *testing.T) {
	h := start(t, Options{})
	resp := h.json("GET", "/api/user-settings", "", 200)
	if resp["version"] != 1.0 || len(resp["client"].(map[string]any)) != 0 {
		t.Fatalf("%v", resp)
	}
	h.json("PUT", "/api/user-settings", `{"client":{"theme":"dark","n":1}}`, 200)
	resp = h.json("PUT", "/api/user-settings", `{"client":{"theme":"light"}}`, 200)
	client := resp["client"].(map[string]any)
	if client["theme"] != "light" || client["n"] != 1.0 {
		t.Fatalf("%v", resp)
	}
	if got := h.json("GET", "/api/user-settings", "", 200)["client"].(map[string]any)["theme"]; got != "light" {
		t.Fatalf("not persisted: %v", got)
	}
	for _, body := range []string{`{"client":[]}`, `[]`, `nope`} {
		if resp := h.json("PUT", "/api/user-settings", body, 400); resp["error"] != "Invalid user settings payload" {
			t.Fatalf("%v", resp)
		}
	}
}

func TestStdinMode(t *testing.T) {
	diff := "diff --git a/x.txt b/x.txt\n--- a/x.txt\n+++ b/x.txt\n@@ -1 +1 @@\n-a\n+b\n"
	h := start(t, Options{StdinDiff: diff, RepoPath: t.TempDir()})
	resp := h.json("GET", "/api/diff?base=whatever", "", 200)
	if resp["openInEditorAvailable"] != false || resp["baseCommitish"] != "stdin" || resp["targetCommitish"] != "stdin" ||
		resp["requestedTargetCommitish"] != "stdin" || resp["commit"] != "stdin diff" {
		t.Fatalf("%v", resp)
	}
	if resp["requestedBaseCommitish"] != "whatever" {
		t.Fatalf("requested base %v", resp["requestedBaseCommitish"])
	}
	h.json("GET", "/api/generated-status/x.txt", "", 400)
	h.json("GET", "/api/revisions", "", 400)
	h.json("GET", "/api/line-count/x.txt", "", 404)
	h.json("GET", "/api/blob/x.txt", "", 404)
	h.json("POST", "/api/open-in-editor", `{"filePath":"x.txt"}`, 400)
}

func TestInvalidCommit(t *testing.T) {
	r := newRepo(t)
	_, err := New(Options{RepoPath: r.Dir, Selection: types.NewDiffSelection("HEAD", "nope", ""), ContextLines: -1})
	if err == nil || err.Error() != "Invalid or non-existent commit: nope" {
		t.Fatalf("got %v", err)
	}
}

func TestCORSAndStatic(t *testing.T) {
	static := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>hello")}}
	h := start(t, Options{Static: static})
	status, header, body := h.do("GET", "/", "", "")
	if status != 200 || !strings.Contains(string(body), "hello") {
		t.Fatalf("%d %q", status, body)
	}
	if header.Get("Access-Control-Allow-Origin") != "http://localhost:*" ||
		header.Get("Access-Control-Allow-Methods") != "GET, POST, PUT, DELETE, OPTIONS" ||
		header.Get("Access-Control-Allow-Headers") != "Origin, X-Requested-With, Content-Type, Accept" {
		t.Fatalf("headers %v", header)
	}
	if status, _, _ := h.do("GET", "/missing.js", "", ""); status != 404 {
		t.Fatalf("missing: %d", status)
	}
}

func TestOpenInEditor(t *testing.T) {
	type call struct {
		command string
		args    []string
	}
	var calls []call
	var launchErr error
	h := start(t, Options{LaunchEditor: func(command string, args []string) error {
		calls = append(calls, call{command, args})
		return launchErr
	}})
	t.Setenv("DIFIT_EDITOR", "")
	t.Setenv("EDITOR", "")
	full := filepath.Join(h.repo.Dir, "src", "a.ts")

	h.json("POST", "/api/open-in-editor", `{"filePath":"src/a.ts","line":"12","editor":{"id":"zed","command":" myed ","argsTemplate":"--goto \"%file:%line\""}}`, 200)
	if len(calls) != 1 || calls[0].command != "myed" || !slices.Equal(calls[0].args, []string{"--goto", full + ":12"}) {
		t.Fatalf("calls %v", calls)
	}

	t.Setenv("DIFIT_EDITOR", "textmate")
	h.json("POST", "/api/open-in-editor", `{"filePath":"src/a.ts","line":0}`, 200)
	if calls[1].command != "mate" || !slices.Equal(calls[1].args, []string{"-l", "1", full}) {
		t.Fatalf("preset: %v", calls[1])
	}

	cases := []struct{ body, want string }{
		{`{"filePath":"src/a.ts","editor":{"id":"None"}}`, "Open in editor is disabled"},
		{`{"filePath":"src/a.ts","editor":{"id":"custom","command":"x"}}`, "Custom editor is not configured. Set a command and arguments in Settings > System."},
		{`{"filePath":"src/a.ts","editor":{"id":"zed","command":"","argsTemplate":"%file"}}`, "Open in editor is not configured"},
		{`{"filePath":42}`, "Invalid request payload"},
		{`{"filePath":"../x"}`, "File path outside repository"},
	}
	for _, c := range cases {
		if resp := h.json("POST", "/api/open-in-editor", c.body, 400); resp["error"] != c.want {
			t.Errorf("%s: %v", c.body, resp)
		}
	}

	launchErr = exec.ErrNotFound
	resp := h.json("POST", "/api/open-in-editor", `{"filePath":"src/a.ts"}`, 500)
	if resp["error"] != `Failed to launch editor: command "mate" is not available on PATH` {
		t.Fatalf("%v", resp)
	}
}

func TestParseLeadingInt(t *testing.T) {
	cases := map[any]int{"12abc": 12, " 7": 7, json.Number("3.9"): 3, "-2": -2, "x": 0, true: 0, nil: 0}
	for in, want := range cases {
		if got := parseLeadingInt(in); got != want {
			t.Errorf("%v: %d", in, got)
		}
	}
}

func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if blank, _ := r.ReadString('\n'); blank != "\n" {
		t.Fatalf("expected blank line after %q", line)
	}
	return strings.TrimSuffix(line, "\n")
}

func TestWatchStreamsCommentChanges(t *testing.T) {
	h := start(t, Options{})
	resp, err := http.Get(h.http.URL + "/api/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("headers %v", resp.Header)
	}
	r := bufio.NewReader(resp.Body)
	if ev := readEvent(t, r); !strings.Contains(ev, `"type":"connected"`) {
		t.Fatalf("got %s", ev)
	}
	h.json("POST", "/api/comment-imports", agentImport, 200)
	if ev := readEvent(t, r); !strings.HasPrefix(ev, `data: {"type":"commentsChanged","version":1,`) {
		t.Fatalf("got %s", ev)
	}
}

func openHeartbeat(t *testing.T, h *harness) {
	t.Helper()
	resp, err := http.Get(h.http.URL + "/api/heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, bufio.NewReader(resp.Body)); ev != "data: connected" {
		t.Fatalf("got %q", ev)
	}
	resp.Body.Close()
	http.DefaultClient.CloseIdleConnections()
}

func TestHeartbeatDisconnectRequestsShutdown(t *testing.T) {
	h := start(t, Options{})
	openHeartbeat(t, h)
	select {
	case <-h.server.ShutdownRequested():
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown not requested")
	}
	if !strings.Contains(h.stdout.String(), "Client disconnected, shutting down server...") {
		t.Fatalf("stdout %q", h.stdout.String())
	}
}

func TestHeartbeatKeepAlive(t *testing.T) {
	h := start(t, Options{KeepAlive: true})
	openHeartbeat(t, h)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(h.stdout.String(), "Press Ctrl+C to stop the server") {
		if time.Now().After(deadline) {
			t.Fatalf("stdout %q", h.stdout.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-h.server.ShutdownRequested():
		t.Fatal("keep-alive server requested shutdown")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStartListensAndWarnsForExternalHost(t *testing.T) {
	r := newRepo(t)
	var stdout, stderr syncBuffer
	var opened string
	s, err := Start(Options{
		RepoPath:      r.Dir,
		Selection:     types.NewDiffSelection("HEAD^", "HEAD", ""),
		ContextLines:  -1,
		Host:          "0.0.0.0",
		PreferredPort: freePort(t),
		OpenBrowser:   true,
		OpenURL:       func(url string) error { opened = url; return errors.New("no browser") },
		DiffMode:      types.DiffModeSpecific,
		Stdout:        &stdout,
		Stderr:        &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.URL != "http://localhost:"+strconv.Itoa(s.Port) || opened != s.URL {
		t.Fatalf("url %s opened %s", s.URL, opened)
	}
	errText := stderr.String()
	for _, want := range []string{"WARNING: Server is accessible from external network!", "Binding to: 0.0.0.0:", "Failed to open browser automatically"} {
		if !strings.Contains(errText, want) {
			t.Errorf("stderr missing %q: %s", want, errText)
		}
	}
	if !strings.Contains(stdout.String(), "File watching disabled") {
		t.Errorf("stdout %q", stdout.String())
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(s.Port) + "/api/comments-json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestListenFallsBackWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	var logged string
	ln, url, err := Listen("127.0.0.1", busyPort, func(format string, args ...any) {
		logged = format
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	gotPort := ln.Addr().(*net.TCPAddr).Port
	if gotPort == busyPort || !strings.HasSuffix(url, ":"+strconv.Itoa(gotPort)) {
		t.Fatalf("url %q port %d", url, gotPort)
	}
	if logged != "Port %d is busy, trying %d...\n" {
		t.Fatalf("logged %q", logged)
	}
}
