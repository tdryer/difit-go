//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"difit-go/internal/testrepo"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds difit once per test run.
func binary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the difit binary")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "difit-cli-test")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "difit")
		out, err := exec.Command("go", "build", "-o", binPath, "difit-go/cmd/difit").CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("%s", out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
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

type process struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan struct{}
}

func startDifit(t *testing.T, dir string, stdin io.Reader, args ...string) *process {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Env = append(os.Environ(), "DIFIT_CONFIG_DIR="+t.TempDir())
	p := &process{t: t, cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-p.done
	})
	return p
}

func (p *process) waitFor(text string) {
	p.t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(p.stdout.String(), text) {
		select {
		case <-deadline:
			p.t.Fatalf("timed out waiting for %q\nstdout:\n%s\nstderr:\n%s", text, p.stdout, p.stderr)
		case <-p.done:
			if !strings.Contains(p.stdout.String(), text) {
				p.t.Fatalf("exited before %q\nstdout:\n%s\nstderr:\n%s", text, p.stdout, p.stderr)
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (p *process) wait() int {
	p.t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		p.t.Fatalf("process did not exit\nstdout:\n%s", p.stdout)
	}
	return -1
}

func runDifit(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}

const sampleDiff = "diff --git a/x.txt b/x.txt\n--- a/x.txt\n+++ b/x.txt\n@@ -1 +1 @@\n-a\n+b\n"

func TestStdinDiffWithCommentCommands(t *testing.T) {
	port := strconv.Itoa(freePort(t))
	p := startDifit(t, t.TempDir(), strings.NewReader(sampleDiff), "--no-open", "--port", port)
	p.waitFor("Press Ctrl+C to stop the server")
	if !strings.Contains(p.stdout.String(), "🚀 difit server started on http://localhost:"+port+"\n📋 Reviewing: diff from stdin\n") {
		t.Fatalf("banner:\n%s", p.stdout)
	}

	code, out, errOut := runDifit(t, `[{"type":"thread","id":"t1","filePath":"x.txt","position":{"side":"new","line":1},"body":"Why b?"}]`,
		"comment", "add", "--port", port)
	var added struct {
		Success  bool     `json:"success"`
		ImportID string   `json:"importId"`
		Count    int      `json:"count"`
		Warnings []string `json:"warnings"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &added) != nil || !added.Success || added.Count != 1 || len(added.ImportID) != 64 {
		t.Fatalf("add: %d %q %q", code, out, errOut)
	}

	code, out, _ = runDifit(t, "", "comment", "get", "--port", port)
	if code != 0 || !strings.Contains(out, "x.txt:L1\nWhy b?") {
		t.Fatalf("get text: %d %q", code, out)
	}
	code, out, _ = runDifit(t, "", "comment", "get", "--port", port, "--format", "json")
	if code != 0 || !strings.HasPrefix(out, `{"version":1,"threads":[{"id":"t1",`) {
		t.Fatalf("get json: %d %q", code, out)
	}

	code, out, _ = runDifit(t, "", "comment", "resolve", "--port", port, "t1", "missing")
	if code != 1 || out != `{"success":false,"resolved":["t1"],"notFound":["missing"],"errors":[]}`+"\n" {
		t.Fatalf("resolve: %d %q", code, out)
	}
	runDifit(t, `{"type":"thread","filePath":"x.txt","position":{"side":"old","line":1},"body":"Final"}`, "comment", "add", "--port", port)

	p.cmd.Process.Signal(syscall.SIGINT)
	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(p.stdout.String(), "👋 Shutting down difit server...\n\n📝 Comments from review session:") ||
		!strings.Contains(p.stdout.String(), "x.txt:L1 (old)\nFinal") {
		t.Fatalf("shutdown output:\n%s", p.stdout)
	}
}

func newRepo(t *testing.T) *testrepo.Repo {
	r := testrepo.New(t)
	r.Write("a.txt", "one\n")
	r.Commit("first")
	r.Write("a.txt", "two\n")
	r.Commit("second")
	return r
}

func TestBrowserDisconnectShutsDown(t *testing.T) {
	r := newRepo(t)
	port := strconv.Itoa(freePort(t))
	p := startDifit(t, r.Dir, nil, "--no-open", "--port", port, "--comment",
		`{"type":"thread","filePath":"a.txt","position":{"side":"new","line":1},"body":"Imported"}`)
	p.waitFor("💡 Use --open to automatically open browser")

	resp, err := http.Get("http://localhost:" + port + "/api/heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "data: connected\n" {
		t.Fatalf("heartbeat %q", line)
	}
	resp.Body.Close()
	http.DefaultClient.CloseIdleConnections()

	if code := p.wait(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := p.stdout.String()
	if !strings.Contains(out, "Client disconnected, shutting down server...\n\n📝 Comments from review session:") ||
		!strings.Contains(out, "a.txt:L1\nImported") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestUntrackedFilesPrompt(t *testing.T) {
	r := newRepo(t)
	r.Write("new.txt", "fresh\n")
	port := strconv.Itoa(freePort(t))
	p := startDifit(t, r.Dir, strings.NewReader("y\n"), ".", "--no-open", "--port", port)
	p.waitFor("🚀 difit server started")
	out := p.stdout.String()
	for _, want := range []string{
		"\n📝 Found 1 untracked file(s):\n    - new.txt\n",
		"(Y/n): ✅ Files added with --intent-to-add\n   💡 To undo this, run `git reset -- new.txt`\n",
		"📋 Reviewing: .\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if status := r.Git("status", "--porcelain"); status != "A new.txt" {
		t.Fatalf("status %q", status)
	}
}

func TestNoDifferences(t *testing.T) {
	r := newRepo(t)
	port := strconv.Itoa(freePort(t))
	p := startDifit(t, r.Dir, nil, "staged", "--port", port)
	p.waitFor("if you want to check manually.")
	if !strings.Contains(p.stdout.String(), "No differences found. Browser will not open automatically.") {
		t.Fatalf("%s", p.stdout)
	}
}

func TestBackgroundMode(t *testing.T) {
	r := newRepo(t)
	port := freePort(t)
	cmd := exec.Command(binary(t), "--background", "--port", strconv.Itoa(port), "HEAD")
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	var info struct {
		Port int    `json:"port"`
		URL  string `json:"url"`
		PID  int    `json:"pid"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatalf("%v: %q", err, stdout.String())
	}
	if !strings.HasPrefix(stdout.String(), `{"port":`) || info.Port != port || info.URL != "http://localhost:"+strconv.Itoa(port) {
		t.Fatalf("handshake %q", stdout.String())
	}
	child, err := os.FindProcess(info.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()

	// The server keeps running after the parent exits and survives
	// browser disconnects (keep-alive is implied).
	resp, err := http.Get(info.URL + "/api/heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(300 * time.Millisecond)
	resp, err = http.Get(info.URL + "/api/diff")
	if err != nil {
		t.Fatalf("server gone after disconnect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBackgroundStartupFailure(t *testing.T) {
	r := newRepo(t)
	cmd := exec.Command(binary(t), "--background", "nope")
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Run()
	if cmd.ProcessState.ExitCode() != 1 || stderr.String() != "Error: Invalid or non-existent commit: nope\n" {
		t.Fatalf("%d %q %q", cmd.ProcessState.ExitCode(), stdout.String(), stderr.String())
	}
}
