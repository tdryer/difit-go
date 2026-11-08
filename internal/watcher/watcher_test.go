package watcher

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"difit-go/internal/testrepo"
	"difit-go/internal/types"
)

const debounce = 50 * time.Millisecond

func decode(t *testing.T, data []byte) types.WatchEvent {
	t.Helper()
	s := string(data)
	if !strings.HasPrefix(s, "data: ") || !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("bad SSE frame %q", s)
	}
	var ev types.WatchEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(s[6:])), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func expectEvent(t *testing.T, ch <-chan []byte) types.WatchEvent {
	t.Helper()
	select {
	case data := <-ch:
		return decode(t, data)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return types.WatchEvent{}
}

func expectNoEvent(t *testing.T, ch <-chan []byte) {
	t.Helper()
	select {
	case data := <-ch:
		t.Fatalf("unexpected event %s", data)
	case <-time.After(400 * time.Millisecond):
	}
}

// settle drains the reloads that follow one change. Windows may report a
// single write as several modifications spread over more than the 50ms test
// debounce, which is not what the negative checks are about.
func settle(t *testing.T, ch <-chan []byte) {
	t.Helper()
	for {
		select {
		case data := <-ch:
			if ev := decode(t, data); ev.Type != "reload" {
				t.Fatalf("unexpected event %+v", ev)
			}
		case <-time.After(400 * time.Millisecond):
			return
		}
	}
}

func start(t *testing.T, mode types.DiffMode) (*testrepo.Repo, *Service, <-chan []byte, *int) {
	r := testrepo.New(t)
	r.Write(".gitignore", "ignored/\n*.log\n")
	r.Write("a.txt", "a\n")
	r.Write("ignored/x.txt", "x\n")
	r.Commit("init")

	s := New(io.Discard, io.Discard)
	invalidations := new(int)
	if err := s.Start(mode, r.Dir, debounce, func() { *invalidations++ }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	ch, _ := s.Subscribe()
	ev := expectEvent(t, ch)
	if ev.Type != "connected" || ev.Message != "Connected to file watcher ("+string(mode)+" mode)" {
		t.Fatalf("connected event %+v", ev)
	}
	return r, s, ch, invalidations
}

func TestWorkingModeReloadsOnFileChange(t *testing.T) {
	r, _, ch, invalidations := start(t, types.DiffModeWorking)

	r.Write("a.txt", "changed\n")
	ev := expectEvent(t, ch)
	if ev.Type != "reload" || ev.ChangeType != "file" || ev.DiffMode != types.DiffModeWorking ||
		ev.Message != "Changes detected in working mode" || *invalidations != 1 {
		t.Fatalf("reload event %+v (invalidations %d)", ev, *invalidations)
	}
	settle(t, ch)

	r.Write("debug.log", "x")
	r.Write("ignored/y.txt", "y")
	r.Write("node_modules/m/index.js", "m")
	expectNoEvent(t, ch)

	r.Write("newdir/deep/file.txt", "f")
	expectEvent(t, ch)
	r.Write("newdir/deep/file.txt", "g")
	if ev := expectEvent(t, ch); ev.Type != "reload" {
		t.Fatalf("change in new directory: %+v", ev)
	}
}

func TestDefaultModeReloadsOnCommit(t *testing.T) {
	r, _, ch, _ := start(t, types.DiffModeDefault)

	r.Write("a.txt", "edited but not committed\n")
	expectNoEvent(t, ch)

	r.Commit("next")
	if ev := expectEvent(t, ch); ev.ChangeType != "commit" {
		t.Fatalf("%+v", ev)
	}
}

func TestStagedModeReloadsOnIndexChange(t *testing.T) {
	r, _, ch, _ := start(t, types.DiffModeStaged)
	r.Write("a.txt", "staged\n")
	expectNoEvent(t, ch)
	r.Git("add", "a.txt")
	if ev := expectEvent(t, ch); ev.ChangeType != "staging" {
		t.Fatalf("%+v", ev)
	}
}

func TestSpecificModeDoesNotWatch(t *testing.T) {
	r := testrepo.New(t)
	r.Commit("init")
	var out bytes.Buffer
	s := New(&out, io.Discard)
	if err := s.Start(types.DiffModeSpecific, r.Dir, debounce, nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if out.String() != "🔍 File watching disabled (specific commit comparison)\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestBroadcastAndStop(t *testing.T) {
	s := New(io.Discard, io.Discard)
	ch, cancel := s.Subscribe()
	if ev := decode(t, <-ch); ev.Message != "Connected to file watcher (undefined mode)" || ev.DiffMode != types.DiffModeDefault {
		t.Fatalf("%+v", ev)
	}
	s.Broadcast(types.WatchEvent{Type: "commentsChanged", Version: types.Ptr(3), Timestamp: "t"})
	if got := string(<-ch); got != "data: {\"type\":\"commentsChanged\",\"version\":3,\"timestamp\":\"t\"}\n\n" {
		t.Fatalf("got %q", got)
	}
	s.Stop()
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed on Stop")
	}
	cancel()
}

func TestGlobToRegexp(t *testing.T) {
	re := globToRegexp(".git/objects/**")
	if !re.MatchString("/repo/.git/objects/ab/cd") || re.MatchString("/repo/.git/index") {
		t.Fatal("objects pattern")
	}
	re = globToRegexp("node_modules/**")
	if !re.MatchString("/repo/web/node_modules/x/y.js") {
		t.Fatal("node_modules pattern")
	}
	re = globToRegexp("a/*.txt")
	if !re.MatchString("/a/b.txt") || re.MatchString("/a/b/c.txt") {
		t.Fatal("single star")
	}
}
