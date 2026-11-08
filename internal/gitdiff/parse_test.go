package gitdiff

import (
	"strings"
	"testing"
)

func TestParseFileBlockPaths(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		path    string
		oldPath string
		status  string
	}{
		{"added binary", []string{"diff --git a/image.jpg b/image.jpg", "new file mode 100644", "index 0000000..abc123", "--- /dev/null", "+++ b/image.jpg", "Binary files /dev/null and b/image.jpg differ"}, "image.jpg", "", "added"},
		{"deleted binary", []string{"diff --git a/old-image.png b/old-image.png", "deleted file mode 100644", "index abc123..0000000", "--- a/old-image.png", "+++ /dev/null", "Binary files a/old-image.png and /dev/null differ"}, "old-image.png", "", "deleted"},
		{"modified binary", []string{"diff --git a/logo.png b/logo.png", "index abc123..def456 100644", "Binary files a/logo.png and b/logo.png differ"}, "logo.png", "", "modified"},
		{"renamed binary", []string{"diff --git a/old.png b/new.png", "similarity index 100%", "rename from old.png", "rename to new.png"}, "new.png", "old.png", "renamed"},
		{"quoted spaces", []string{`diff --git "a/test with spaces/file name.txt" "b/test with spaces/file name.txt"`, "new file mode 100644", "--- /dev/null", `+++ "b/test with spaces/file name.txt"`, "@@ -0,0 +1 @@", "+foo"}, "test with spaces/file name.txt", "", "added"},
		{"jinja brackets", []string{`diff --git "a/t/{{ name }}/__.py" "b/t/{{ name }}/__.py"`, "new file mode 100644", "--- /dev/null", `+++ "b/t/{{ name }}/__.py"`}, "t/{{ name }}/__.py", "", "added"},
		{"escaped tabs", []string{`diff --git "a/file\twith\ttabs.txt" "b/file\twith\ttabs.txt"`, "new file mode 100644", "--- /dev/null", `+++ "b/file\twith\ttabs.txt"`}, "file\twith\ttabs.txt", "", "added"},
		{"renamed with spaces", []string{`diff --git "a/old folder/old name.txt" "b/new folder/new name.txt"`, "similarity index 100%", "rename from old folder/old name.txt", "rename to new folder/new name.txt"}, "new folder/new name.txt", "old folder/old name.txt", "renamed"},
		{"unquoted", []string{"diff --git a/src/file.js b/src/file.js", "index 1234567..8901234 100644", "--- a/src/file.js", "+++ b/src/file.js", "@@ -1,3 +1,3 @@", " line1", "-old", "+new", " line3"}, "src/file.js", "", "modified"},
		{"unquoted spaces", []string{"diff --git a/path with spaces/file.txt b/path with spaces/file.txt", "--- a/path with spaces/file.txt", "+++ b/path with spaces/file.txt"}, "path with spaces/file.txt", "", "modified"},
		{"japanese", []string{"diff --git a/テスト/file.txt b/テスト/file.txt", "new file mode 100644", "--- /dev/null", "+++ b/テスト/file.txt"}, "テスト/file.txt", "", "added"},
		{"unquoted octal", []string{`diff --git a/some\040folder/file\040name.ts b/some\040folder/file\040name.ts`, `--- a/some\040folder/file\040name.ts`, `+++ b/some\040folder/file\040name.ts`}, "some folder/file name.ts", "", "modified"},
		{"b/ in filename", []string{"diff --git a/dir b/sub/file b/dir b/sub/file", "--- a/dir b/sub/file", "+++ b/dir b/sub/file"}, "dir b/sub/file", "", "modified"},
		{"rename b/ in path", []string{"diff --git a/old b/path/file b/new b/path/file", "similarity index 100%", "rename from old b/path/file", "rename to new b/path/file"}, "new b/path/file", "old b/path/file", "renamed"},
		{"mnemonic prefixes", []string{"diff --git c/a/test.txt w/a/test.txt", "--- c/a/test.txt", "+++ w/a/test.txt\t"}, "a/test.txt", "", "modified"},
		{"mnemonic rename", []string{"diff --git c/old/name.txt w/new/name.txt", "similarity index 100%", "rename from c/old/name.txt\t", "rename to w/new/name.txt\t"}, "new/name.txt", "old/name.txt", "renamed"},
		{"trailing tab", []string{"diff --git c/foo bar.txt w/foo bar.txt", "--- c/foo bar.txt\t", "+++ w/foo bar.txt\t"}, "foo bar.txt", "", "modified"},
		{"utf8 octal", []string{`diff --git "a/file\303\244.txt" "b/file\303\244.txt"`, "new file mode 100644", "--- /dev/null", `+++ "b/file\303\244.txt"`}, "fileä.txt", "", "added"},
		{"mixed escapes", []string{`diff --git "a/dir\303\251/file\twith\nmixed.txt" "b/dir\303\251/file\twith\nmixed.txt"`, "new file mode 100644", "--- /dev/null", `+++ "b/dir\303\251/file\twith\nmixed.txt"`}, "diré/file\twith\nmixed.txt", "", "added"},
		{"new mode wins", []string{"diff --git a/x b/x", "new file mode 100644", "--- a/x", "+++ b/x"}, "x", "", "added"},
		{"deleted mode wins", []string{"diff --git a/x b/x", "deleted file mode 100644", "--- a/x", "+++ b/x"}, "x", "", "deleted"},
	}
	for _, c := range cases {
		f, ok := parseFileBlock(strings.Join(c.lines, "\n"), formatGit)
		if !ok {
			t.Errorf("%s: not parsed", c.name)
			continue
		}
		if f.Path != c.path || f.OldPath != c.oldPath || f.Status != c.status {
			t.Errorf("%s: got path=%q old=%q status=%s, want %q %q %s", c.name, f.Path, f.OldPath, f.Status, c.path, c.oldPath, c.status)
		}
		if f.IsGenerated {
			t.Errorf("%s: unexpectedly generated", c.name)
		}
	}
}

func TestParseChunksLineNumbers(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/f.txt b/f.txt",
		"--- a/f.txt",
		"+++ b/f.txt",
		"@@ -1,3 +1,3 @@ func header",
		" a",
		"-b",
		"+B",
		" c",
		"\\ No newline at end of file",
		"@@ -10 +10,2 @@",
		" x",
		"+y\r",
	}, "\n")
	files := ParseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("got %d files", len(files))
	}
	f := files[0]
	if f.Additions != 2 || f.Deletions != 1 || len(f.Chunks) != 2 {
		t.Fatalf("unexpected counts %+v", f)
	}
	c := f.Chunks[0]
	if c.Header != "@@ -1,3 +1,3 @@ func header" || c.OldLines != 3 || len(c.Lines) != 4 {
		t.Fatalf("chunk 0 = %+v", c)
	}
	if l := c.Lines[1]; l.Type != "delete" || *l.OldLineNumber != 2 || l.NewLineNumber != nil {
		t.Fatalf("delete line = %+v", l)
	}
	if l := c.Lines[2]; l.Type != "add" || l.OldLineNumber != nil || *l.NewLineNumber != 2 {
		t.Fatalf("add line = %+v", l)
	}
	c = f.Chunks[1]
	if c.OldLines != 1 || c.NewLines != 2 || c.Lines[1].Content != "y\r" || *c.Lines[1].NewLineNumber != 11 {
		t.Fatalf("chunk 1 = %+v", c)
	}
}

func TestParseStdinDiffPlainFormats(t *testing.T) {
	plain := "--- file1.txt\t2026-07-11 12:00:00\n+++ file2.txt\t2026-07-11 12:00:01\n@@ -1,2 +1,2 @@\n unchanged\n-old content\n+new content"
	resp := ParseStdinDiff(plain)
	if resp.Commit != "stdin diff" || resp.IsEmpty || len(resp.Files) != 1 {
		t.Fatalf("got %+v", resp)
	}
	if f := resp.Files[0]; f.Path != "file2.txt" || f.Status != "modified" || f.Additions != 1 || f.Deletions != 1 {
		t.Fatalf("got %+v", f)
	}

	crlf := strings.Join([]string{"--- old.txt", "+++ new.txt", "@@ -1 +1 @@", "-old content", "+new content"}, "\r\n")
	files := ParseUnifiedDiff(crlf)
	if len(files) != 1 || files[0].Path != "new.txt" || files[0].Chunks[0].Lines[0].Content != "old content" {
		t.Fatalf("crlf: %+v", files)
	}

	multi := "--- first-old.txt\n+++ first-new.txt\n@@ -1 +1 @@\n-before\n+after\n--- second-old.txt\n+++ second-new.txt\n@@ -1 +1,2 @@\n existing\n+added"
	files = ParseUnifiedDiff(multi)
	if len(files) != 2 || files[0].Path != "first-new.txt" || files[1].Path != "second-new.txt" || files[1].Additions != 1 || files[1].Deletions != 0 {
		t.Fatalf("multi: %+v", files)
	}

	headerLike := "--- old.txt\n+++ new.txt\n@@ -1,2 +1,2 @@\n--- deleted content\n+++ added content"
	files = ParseUnifiedDiff(headerLike)
	if len(files) != 1 || files[0].Additions != 1 || files[0].Deletions != 1 {
		t.Fatalf("header-like: %+v", files)
	}

	addDel := "--- /dev/null\n+++ added.txt\n@@ -0,0 +1 @@\n+added\n--- deleted.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-deleted"
	files = ParseUnifiedDiff(addDel)
	if len(files) != 2 || files[0].Status != "added" || files[0].Path != "added.txt" || files[1].Status != "deleted" || files[1].Path != "deleted.txt" {
		t.Fatalf("add/del: %+v", files)
	}

	if resp := ParseStdinDiff(""); !resp.IsEmpty || resp.Files == nil {
		t.Fatalf("empty: %+v", resp)
	}
}

func TestParseMultipleGitFiles(t *testing.T) {
	diff := "preamble\ndiff --git a/a.min.js b/a.min.js\n--- a/a.min.js\n+++ b/a.min.js\n@@ -1 +1 @@\n-x\n+y\ndiff --git a/b.txt b/c.txt\nsimilarity index 90%\nrename from b.txt\nrename to c.txt\n--- a/b.txt\n+++ b/c.txt\n@@ -1 +1 @@\n-p\n+q\n"
	files := ParseUnifiedDiff(diff)
	if len(files) != 2 {
		t.Fatalf("got %d files", len(files))
	}
	if !files[0].IsGenerated || files[1].Status != "renamed" || files[1].OldPath != "b.txt" || files[1].Path != "c.txt" {
		t.Fatalf("got %+v", files)
	}
}

func TestValidateCommitish(t *testing.T) {
	valid := []string{"HEAD", "HEAD~3", "HEAD^", "HEAD^2~1", "@", "@~1", "abc123", "main", "feature/x", "origin/main", "v1.0.0", ".", "working", "staged", "main^"}
	invalid := []string{"", " ", "-main", "a..b", "a@{1}", "a//b", "/a", "a/", "a.lock", "a b", "a:b", "a?", ".hidden", "x/.y", "~1"}
	for _, v := range valid {
		if !ValidateCommitish(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range invalid {
		if ValidateCommitish(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
}

func TestValidateDiffArguments(t *testing.T) {
	cases := []struct {
		target, base string
		hasBase      bool
		err          string
	}{
		{"HEAD", "HEAD^", true, ""},
		{"working", "staged", true, ""},
		{".", "main", true, ""},
		{"bad..ref", "", false, "Invalid target commit-ish format"},
		{"HEAD", "bad..ref", true, "Invalid base commit-ish format"},
		{"HEAD", ".", true, "Special arguments (working, staged, .) are only allowed as target, not base. Got base: ."},
		{"HEAD", "HEAD", true, "Cannot compare HEAD with itself"},
		{"working", "HEAD", true, `"working" shows unstaged changes`},
	}
	for _, c := range cases {
		err := ValidateDiffArguments(c.target, c.base, c.hasBase)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.HasPrefix(err.Error(), c.err)) {
			t.Errorf("%s vs %s: got %v, want %q", c.target, c.base, err, c.err)
		}
	}
}
