package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/tdryer/difit-go/internal/types"
)

func TestResolveDiffSelection(t *testing.T) {
	cases := []struct {
		commitish, compareWith string
		mergeBase              bool
		want                   types.DiffSelection
	}{
		{"HEAD", "", false, types.NewDiffSelection("HEAD^", "HEAD", "")},
		{"working", "", false, types.NewDiffSelection("staged", "working", "")},
		{"staged", "", false, types.NewDiffSelection("HEAD", "staged", "")},
		{".", "", false, types.NewDiffSelection("HEAD", ".", "")},
		{".", "main", false, types.NewDiffSelection("main", ".", "")},
		{"feature", "main", true, types.NewDiffSelection("main", "feature", types.BaseModeMergeBase)},
	}
	for _, c := range cases {
		if got := resolveDiffSelection(c.commitish, c.compareWith, c.mergeBase); got != c.want {
			t.Errorf("%s %s: %+v", c.commitish, c.compareWith, got)
		}
	}
}

func TestDetermineDiffMode(t *testing.T) {
	cases := []struct {
		target, compareWith string
		want                types.DiffMode
	}{
		{"HEAD", "", types.DiffModeDefault},
		{"abc123", "", types.DiffModeDefault},
		{"HEAD", "main", types.DiffModeDefault},
		{".", "main", types.DiffModeDot},
		{"feature", "main", types.DiffModeSpecific},
		{"working", "", types.DiffModeWorking},
		{"staged", "", types.DiffModeStaged},
		{".", "", types.DiffModeDot},
	}
	for _, c := range cases {
		sel := types.NewDiffSelection("x", c.target, "")
		if got := determineDiffMode(sel, c.compareWith); got != c.want {
			t.Errorf("%s %s: %s", c.target, c.compareWith, got)
		}
	}
}

func TestShouldReadStdin(t *testing.T) {
	src := func(s stdinSource) func() stdinSource { return func() stdinSource { return s } }
	cases := []struct {
		commitish  string
		positional bool
		source     stdinSource
		want       bool
	}{
		{"-", true, stdinTTY, true},
		{"HEAD", true, stdinPipe, false},
		{"HEAD", false, stdinPipe, true},
		{"HEAD", false, stdinFile, true},
		{"HEAD", false, stdinSocket, true},
		{"HEAD", false, stdinTTY, false},
	}
	for _, c := range cases {
		if got := shouldReadStdin(c.commitish, c.positional, src(c.source)); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

func TestParseIntJS(t *testing.T) {
	cases := map[string]struct {
		n  int
		ok bool
	}{"3": {3, true}, " 12x": {12, true}, "-1": {-1, true}, "abc": {0, false}, "": {0, false}, "+": {0, false}}
	for in, want := range cases {
		if n, ok := parseIntJS(in); n != want.n || ok != want.ok {
			t.Errorf("%q: %d %v", in, n, ok)
		}
	}
}

func TestBackgroundChildArgs(t *testing.T) {
	got := backgroundChildArgs([]string{"HEAD~2", "--background", "--port", "5000"})
	if !slices.Equal(got, []string{"HEAD~2", "--port", "5000", "--keep-alive", "--no-open"}) {
		t.Fatalf("got %v", got)
	}
	got = backgroundChildArgs([]string{"--no-open", "--keep-alive", "--background"})
	if !slices.Equal(got, []string{"--no-open", "--keep-alive"}) {
		t.Fatalf("got %v", got)
	}
}

func runApp(t *testing.T, stdin string, source stdinSource, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := &app{
		stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr, version: "1.2.3",
		stdinSource: func() stdinSource { return source },
	}
	code := a.run(args)
	return code, stdout.String(), stderr.String()
}

func TestValidationErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		args   []string
		stdin  string
		source stdinSource
		want   string
	}{
		{[]string{"--context", "-1"}, "", stdinTTY, "Error: --context must be a non-negative integer\n"},
		{[]string{"--context", "x"}, "", stdinTTY, "Error: --context must be a non-negative integer\n"},
		{[]string{"--pr", "https://github.com/o/r/pull/1", "main"}, "", stdinTTY, "Error: --pr option cannot be used with positional arguments\n"},
		{[]string{"--pr", "u", "--merge-base"}, "", stdinTTY, "Error: --merge-base option cannot be used with --pr\n"},
		{[]string{"--pr", "u", "--context", "2"}, "", stdinTTY, "Error: --context option cannot be used with --pr\n"},
		{[]string{"--context", "2"}, "diff", stdinPipe, "Error: --context option cannot be used with stdin diff\n"},
		{[]string{"-", "--merge-base"}, "diff", stdinTTY, "Error: --merge-base option cannot be used with stdin diff\n"},
		{[]string{"-"}, "  \n", stdinTTY, "Error: No diff content received from stdin\n"},
		{[]string{"working", "--merge-base"}, "", stdinTTY, "Error: --merge-base requires a commit-ish base, but resolved base was \"staged\"\n"},
		{[]string{"--comment", "{"}, "", stdinTTY, "Error: Invalid --comment JSON\n"},
		{[]string{"--comment", `{"type":"x"}`}, "", stdinTTY, "Error: Invalid comment import field: type\n"},
	}
	for _, c := range cases {
		code, _, stderr := runApp(t, c.stdin, c.source, c.args...)
		if code != 1 || stderr != c.want {
			t.Errorf("%v: code %d stderr %q", c.args, code, stderr)
		}
	}
}

func TestVersion(t *testing.T) {
	for _, flag := range []string{"-v", "--version"} {
		code, stdout, _ := runApp(t, "", stdinTTY, flag)
		if code != 0 || stdout != "1.2.3\n" {
			t.Errorf("%s: %d %q", flag, code, stdout)
		}
	}
}

func TestCommentCommandErrors(t *testing.T) {
	code, _, stderr := runApp(t, "", stdinTTY, "comment", "add", "--port", "1")
	if code != 1 || stderr != "Error: Provide comment JSON as an argument or via stdin\n" {
		t.Errorf("%d %q", code, stderr)
	}
	code, _, stderr = runApp(t, " ", stdinPipe, "comment", "add", "--port", "1")
	if code != 1 || stderr != "Error: No comment JSON received from stdin\n" {
		t.Errorf("%d %q", code, stderr)
	}
	code, _, stderr = runApp(t, "", stdinTTY, "comment", "get", "--port", "1", "--format", "xml")
	if code != 1 || !strings.Contains(stderr, "Allowed choices are text, json.") {
		t.Errorf("%d %q", code, stderr)
	}
	code, _, stderr = runApp(t, "", stdinTTY, "comment", "resolve", "--port", "1", "t1")
	if code != 1 || stderr != "Error: Cannot connect to difit server on port 1. Is the server running?\n" {
		t.Errorf("%d %q", code, stderr)
	}
}
