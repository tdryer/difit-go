package gitdiff

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"difit-go/internal/testrepo"
	"difit-go/internal/types"
)

func setupRepo(t *testing.T) (*testrepo.Repo, *Parser) {
	r := testrepo.New(t)
	r.Write("a.txt", "one\ntwo\nthree\n")
	r.Write("b.txt", "keep\n")
	r.Commit("first")
	r.Write("a.txt", "one\nTWO\nthree\n")
	r.Write("c.txt", "new\n")
	r.Commit("second")
	return r, NewParser(r.Dir)
}

func fileNames(files []types.DiffFile) []string {
	var names []string
	for _, f := range files {
		names = append(names, f.Status+":"+f.Path)
	}
	return names
}

func TestParseDiffCommitRange(t *testing.T) {
	r, p := setupRepo(t)
	resp, err := p.ParseDiff(types.NewDiffSelection("HEAD^", "HEAD", ""), false, -1)
	if err != nil {
		t.Fatal(err)
	}
	head := r.Git("rev-parse", "--short=7", "HEAD")
	parent := r.Git("rev-parse", "--short=7", "HEAD^")
	if resp.Commit != parent+"..."+head || resp.BaseCommitish != parent || resp.TargetCommitish != head {
		t.Fatalf("got %+v", resp)
	}
	if resp.RequestedBaseCommitish != "HEAD^" || resp.RequestedTargetCommitish != "HEAD" || resp.RequestedBaseMode != "" {
		t.Fatalf("requested fields: %+v", resp)
	}
	if got := fileNames(resp.Files); !slices.Equal(got, []string{"modified:a.txt", "added:c.txt"}) {
		t.Fatalf("files %v", got)
	}
}

func TestParseDiffWorkingStagedDot(t *testing.T) {
	r, p := setupRepo(t)
	r.Write("a.txt", "one\nTWO\nthree\nfour\n")
	r.Git("add", "a.txt")
	r.Write("b.txt", "keep\nmore\n")

	working, err := p.ParseDiff(types.NewDiffSelection("staged", "working", ""), false, -1)
	if err != nil {
		t.Fatal(err)
	}
	if working.Commit != "Working Directory (unstaged changes)" || working.BaseCommitish != "staged" || working.TargetCommitish != "working" {
		t.Fatalf("working: %+v", working)
	}
	if got := fileNames(working.Files); !slices.Equal(got, []string{"modified:b.txt"}) {
		t.Fatalf("working files %v", got)
	}

	staged, err := p.ParseDiff(types.NewDiffSelection("HEAD", "staged", ""), false, -1)
	if err != nil {
		t.Fatal(err)
	}
	head := r.Git("rev-parse", "--short=7", "HEAD")
	if staged.Commit != head+" vs Staging Area (staged changes)" || staged.BaseCommitish != head {
		t.Fatalf("staged: %+v", staged)
	}
	if got := fileNames(staged.Files); !slices.Equal(got, []string{"modified:a.txt"}) {
		t.Fatalf("staged files %v", got)
	}

	dot, err := p.ParseDiff(types.NewDiffSelection("HEAD", ".", ""), false, -1)
	if err != nil {
		t.Fatal(err)
	}
	if dot.Commit != head+" vs Working Directory (all uncommitted changes)" || len(dot.Files) != 2 {
		t.Fatalf("dot: %+v", dot)
	}
}

func TestParseDiffOptions(t *testing.T) {
	r, p := setupRepo(t)
	r.Write("a.txt", "one\nTWO \nthree\n")
	r.Write("long.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n")
	r.Commit("long")
	r.Write("long.txt", "1\n2\n3\n4\nfive\n6\n7\n8\n9\n")

	resp, err := p.ParseDiff(types.NewDiffSelection("HEAD", ".", ""), false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.Files[0].Chunks[0].Lines); n != 4 {
		t.Fatalf("-U1 gave %d lines", n)
	}

	r.Write("long.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n")
	r.Write("a.txt", "one\nTWO  \nthree\n")
	resp, err = p.ParseDiff(types.NewDiffSelection("HEAD", ".", ""), true, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 0 || !resp.IsEmpty {
		t.Fatalf("whitespace-only change not ignored: %v", fileNames(resp.Files))
	}
}

func TestParseDiffMergeBase(t *testing.T) {
	r, p := setupRepo(t)
	forkPoint := r.Git("rev-parse", "--short=7", "HEAD")
	r.Git("checkout", "-q", "-b", "feature")
	r.Write("feature.txt", "f\n")
	r.Commit("feature")
	r.Git("checkout", "-q", "main")
	r.Write("main.txt", "m\n")
	r.Commit("main moves on")

	resp, err := p.ParseDiff(types.NewDiffSelection("main", "feature", types.BaseModeMergeBase), false, -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.BaseCommitish != forkPoint || resp.RequestedBaseMode != types.BaseModeMergeBase || resp.RequestedBaseCommitish != "main" {
		t.Fatalf("got %+v", resp)
	}
	if got := fileNames(resp.Files); !slices.Equal(got, []string{"added:feature.txt"}) {
		t.Fatalf("files %v", got)
	}

	if _, err := p.ParseDiff(types.NewDiffSelection("main~1", "feature^", ""), false, -1); err != nil {
		t.Fatalf("revision suffixes: %v", err)
	}
}

func TestParseDiffErrors(t *testing.T) {
	_, p := setupRepo(t)
	_, err := p.ParseDiff(types.NewDiffSelection("HEAD", "nope", ""), false, -1)
	if err == nil || !strings.HasPrefix(err.Error(), "Failed to parse diff for nope vs HEAD: ") {
		t.Fatalf("got %v", err)
	}
	_, err = p.ParseDiff(types.NewDiffSelection("HEAD", "HEAD", ""), false, -1)
	if err == nil || !strings.Contains(err.Error(), "Cannot compare HEAD with itself") {
		t.Fatalf("got %v", err)
	}
}

func TestGitattributesGenerated(t *testing.T) {
	r, p := setupRepo(t)
	r.Write(".gitattributes", "gen/** linguist-generated=true\n")
	r.Write("gen/out.txt", "x\n")
	r.Write("src/main.txt", "y\n")
	r.Commit("attrs")

	resp, err := p.ParseDiff(types.NewDiffSelection("HEAD^", "HEAD", ""), false, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range resp.Files {
		if want := f.Path == "gen/out.txt"; f.IsGenerated != want {
			t.Errorf("%s: isGenerated=%v", f.Path, f.IsGenerated)
		}
	}
	if gen, source := p.GetGeneratedStatus("gen/out.txt", "HEAD"); !gen || source != "path" {
		t.Fatalf("gitattributes status: %v %s", gen, source)
	}
}

func TestGeneratedStatusByContent(t *testing.T) {
	r, p := setupRepo(t)
	r.Write("src/api.ts", "// @generated\nexport {}\n")
	r.Commit("api")
	if gen, source := p.GetGeneratedStatus("src/api.ts", "HEAD"); !gen || source != "content" {
		t.Fatalf("got %v %s", gen, source)
	}
	if gen, source := p.GetGeneratedStatus("yarn.lock", "HEAD"); !gen || source != "path" {
		t.Fatalf("got %v %s", gen, source)
	}
	if gen, source := p.GetGeneratedStatus("missing.ts", "HEAD"); gen || source != "path" {
		t.Fatalf("got %v %s", gen, source)
	}
}

func TestBlobContent(t *testing.T) {
	r, p := setupRepo(t)
	r.Write("a.txt", "staged\n")
	r.Git("add", "a.txt")
	r.Write("a.txt", "working\n")
	r.Write("dir with space/x.txt", "spaced")

	cases := []struct{ ref, want string }{
		{"HEAD", "one\nTWO\nthree\n"},
		{"HEAD^", "one\ntwo\nthree\n"},
		{"staged", "staged\n"},
		{"working", "working\n"},
		{".", "working\n"},
	}
	for _, c := range cases {
		got, err := p.GetBlobContent("a.txt", c.ref)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q, %v", c.ref, got, err)
		}
	}
	if got, err := p.GetBlobContent("dir with space/x.txt", "working"); err != nil || string(got) != "spaced" {
		t.Errorf("spaced path: %q %v", got, err)
	}

	if _, err := p.GetBlobContent("../outside", "HEAD"); err != ErrPathOutsideRepo {
		t.Errorf("traversal at ref: %v", err)
	}
	if _, err := p.GetBlobContent("/etc/passwd", "HEAD"); err != ErrPathOutsideRepo {
		t.Errorf("absolute at ref: %v", err)
	}
	if _, err := p.GetBlobContent("../outside", "working"); err == nil {
		t.Error("expected traversal in working tree to fail")
	}
	if _, err := p.GetBlobContent("/etc/hostname", "working"); err != ErrPathOutsideRepo && err == nil {
		t.Error("expected absolute working tree path to fail")
	}

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.Dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetBlobContent("link.txt", "working"); err != ErrPathOutsideRepo {
		t.Errorf("symlink escape: %v", err)
	}

	_, err := p.GetBlobContent("missing.txt", "HEAD")
	if err == nil || !strings.HasPrefix(err.Error(), "Failed to get blob content for missing.txt at HEAD: ") {
		t.Errorf("missing: %v", err)
	}
}

func TestBlobTooLarge(t *testing.T) {
	r, p := setupRepo(t)
	r.Write("big.bin", strings.Repeat("x", maxBlobBytes+1))
	r.Commit("big")
	_, err := p.GetBlobContent("big.bin", "HEAD")
	if err == nil || err.Error() != "Image file big.bin is too large to display (over 10MB limit)" {
		t.Fatalf("got %v", err)
	}
}

func TestLineCount(t *testing.T) {
	r, p := setupRepo(t)
	r.Write("nonl.txt", "a\nb")
	r.Write("empty.txt", "")
	for path, want := range map[string]int{"a.txt": 3, "nonl.txt": 2, "empty.txt": 0} {
		if got, err := p.GetLineCount(path, "working"); err != nil || got != want {
			t.Errorf("%s: got %d, %v; want %d", path, got, err, want)
		}
	}
}

func TestRevisionOptions(t *testing.T) {
	r, p := setupRepo(t)
	r.Git("branch", "Zeta")
	r.Git("branch", "alpha")
	r.Git("checkout", "-q", "-b", "work")

	opts, err := p.GetRevisionOptions("HEAD^", "working")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range opts.Branches {
		names = append(names, b.Name)
	}
	if !slices.Equal(names, []string{"main", "work", "alpha", "Zeta"}) {
		t.Fatalf("branches %v", names)
	}
	if !opts.Branches[1].Current || opts.Branches[0].Current {
		t.Fatalf("current flags %+v", opts.Branches)
	}
	if len(opts.Commits) != 2 || opts.Commits[0].Message != "second" || len(opts.Commits[0].ShortHash) != 7 {
		t.Fatalf("commits %+v", opts.Commits)
	}
	if opts.ResolvedBase != r.Git("rev-parse", "--short=7", "HEAD^") || opts.ResolvedTarget != "" {
		t.Fatalf("resolved %q %q", opts.ResolvedBase, opts.ResolvedTarget)
	}
	if opts.OriginDefaultBranch != "" {
		t.Fatalf("no remote, got %q", opts.OriginDefaultBranch)
	}
}

func TestOriginDefaultBranch(t *testing.T) {
	r, p := setupRepo(t)
	head := r.Git("rev-parse", "HEAD")
	r.Git("update-ref", "refs/remotes/origin/master", head)
	if got := p.GetOriginDefaultBranch(); got != "origin/master" {
		t.Fatalf("fallback: %q", got)
	}
	r.Git("update-ref", "refs/remotes/origin/develop", head)
	r.Git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
	if got := p.GetOriginDefaultBranch(); got != "origin/develop" {
		t.Fatalf("origin/HEAD: %q", got)
	}
	if got := p.GetDefaultBranch(); got != "develop" {
		t.Fatalf("default branch: %q", got)
	}
}

func TestValidateCommitAndUntracked(t *testing.T) {
	r, p := setupRepo(t)
	if !p.ValidateCommit("HEAD") || !p.ValidateCommit("working") || p.ValidateCommit("nope") {
		t.Fatal("ValidateCommit mismatch")
	}
	r.Write("new dir/u1.txt", "u")
	r.Write("u2.txt", "u")
	files, err := p.FindUntrackedFiles()
	if err != nil || !slices.Equal(files, []string{"new dir/u1.txt", "u2.txt"}) {
		t.Fatalf("untracked %v %v", files, err)
	}
	if err := p.MarkIntentToAdd(files); err != nil {
		t.Fatal(err)
	}
	resp, err := p.ParseDiff(types.NewDiffSelection("staged", "working", ""), false, -1)
	if err != nil || len(resp.Files) != 2 {
		t.Fatalf("intent-to-add files not in diff: %v %v", fileNames(resp.Files), err)
	}
}
