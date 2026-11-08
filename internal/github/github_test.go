package github

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tdryer/difit-go/internal/comments"
)

func comment(id, body, created, updated, login string) map[string]any {
	return map[string]any{"id": id, "body": body, "createdAt": created, "updatedAt": updated, "author": map[string]any{"login": login}}
}

func thread(overrides map[string]any) map[string]any {
	t := map[string]any{
		"id": "THREAD_1", "isResolved": false, "isOutdated": false, "subjectType": "LINE",
		"path": "src/example.ts", "diffSide": "RIGHT", "startDiffSide": nil, "line": 12,
		"startLine": nil, "originalLine": 11, "originalStartLine": nil,
		"comments": map[string]any{"nodes": []any{comment("COMMENT_1", "Imported comment", "2026-03-25T09:00:00Z", "2026-03-25T09:05:00Z", "octocat")}},
	}
	for k, v := range overrides {
		t[k] = v
	}
	return t
}

func response(t *testing.T, hasNext bool, cursor any, threads ...map[string]any) []byte {
	data, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": map[string]any{"nodes": threads, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func importsJSON(t *testing.T, page Page) string {
	data, err := comments.MarshalJSON(page.Imports)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestImportsUnresolvedThreadsSortedAndSkipsOthers(t *testing.T) {
	data := response(t, true, "CURSOR_1",
		thread(map[string]any{"id": "THREAD_UNRESOLVED", "line": 14, "comments": map[string]any{"nodes": []any{
			comment("COMMENT_REPLY", "Second comment", "2026-03-25T09:10:00Z", "2026-03-25T09:12:00Z", "reviewer-2"),
			comment("COMMENT_ROOT", "First comment", "2026-03-25T09:00:00Z", "2026-03-25T09:05:00Z", "reviewer-1"),
		}}}),
		thread(map[string]any{"id": "THREAD_RESOLVED", "isResolved": true}),
		thread(map[string]any{"id": "THREAD_OUTDATED", "isOutdated": true}),
		thread(map[string]any{"id": "THREAD_FILE", "subjectType": "FILE"}),
	)
	page, err := ParseReviewThreadsResponse(data, func(s string) { t.Errorf("warning %s", s) })
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"thread","id":"COMMENT_ROOT","filePath":"src/example.ts","position":{"side":"new","line":14},"body":"First comment","author":"reviewer-1","createdAt":"2026-03-25T09:00:00Z","updatedAt":"2026-03-25T09:05:00Z"},` +
		`{"type":"reply","id":"COMMENT_REPLY","filePath":"src/example.ts","position":{"side":"new","line":14},"body":"Second comment","author":"reviewer-2","createdAt":"2026-03-25T09:10:00Z","updatedAt":"2026-03-25T09:12:00Z"}]`
	if got := importsJSON(t, page); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if !page.HasNextPage || page.EndCursor != "CURSOR_1" {
		t.Fatalf("page info %+v", page)
	}
}

func TestMapsSidesAndRanges(t *testing.T) {
	data := response(t, false, nil,
		thread(map[string]any{"line": 20, "startLine": 20}),
		thread(map[string]any{"line": 24, "startLine": 21, "startDiffSide": "RIGHT"}),
		thread(map[string]any{"diffSide": "LEFT", "line": nil, "originalLine": 8}),
		thread(map[string]any{"diffSide": "LEFT", "line": nil, "originalLine": 11, "originalStartLine": 7, "startDiffSide": "LEFT"}),
	)
	page, err := ParseReviewThreadsResponse(data, func(s string) { t.Errorf("warning %s", s) })
	if err != nil {
		t.Fatal(err)
	}
	var positions []string
	for _, ci := range page.Imports {
		b, _ := json.Marshal(ci.Position)
		positions = append(positions, string(b))
	}
	want := []string{
		`{"side":"new","line":20}`,
		`{"side":"new","line":{"start":21,"end":24}}`,
		`{"side":"old","line":8}`,
		`{"side":"old","line":{"start":7,"end":11}}`,
	}
	if !slices.Equal(positions, want) {
		t.Fatalf("got %v", positions)
	}
}

func TestWarnsOnInvalidLineMapping(t *testing.T) {
	data := response(t, false, nil,
		thread(map[string]any{"id": "THREAD_RIGHT_MISSING_LINE", "line": nil}),
		thread(map[string]any{"id": "THREAD_LEFT_MISSING_ORIGINAL", "diffSide": "LEFT", "line": nil, "originalLine": nil}),
		thread(map[string]any{"id": "THREAD_RIGHT_BAD_RANGE", "line": 9, "startLine": 12, "startDiffSide": "RIGHT"}),
		thread(map[string]any{"id": "THREAD_LEFT_BAD_SIDE", "diffSide": "LEFT", "line": nil, "originalLine": 9, "originalStartLine": 7, "startDiffSide": "RIGHT"}),
		thread(map[string]any{"id": "", "path": " "}),
		thread(map[string]any{"id": "T_EMPTY", "comments": map[string]any{"nodes": []any{}}}),
	)
	var warnings []string
	page, err := ParseReviewThreadsResponse(data, func(s string) { warnings = append(warnings, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Imports) != 0 {
		t.Fatalf("imports %v", page.Imports)
	}
	want := []string{
		"Warning: Skipping PR review thread THREAD_RIGHT_MISSING_LINE: RIGHT thread is missing line.",
		"Warning: Skipping PR review thread THREAD_LEFT_MISSING_ORIGINAL: LEFT thread is missing originalLine.",
		"Warning: Skipping PR review thread THREAD_RIGHT_BAD_RANGE: RIGHT thread has an invalid multi-line range.",
		"Warning: Skipping PR review thread THREAD_LEFT_BAD_SIDE: LEFT thread has mismatched startDiffSide.",
		"Warning: Skipping PR review thread: Thread is missing path.",
		"Warning: Skipping PR review thread T_EMPTY: Thread has no comments.",
	}
	if !slices.Equal(warnings, want) {
		t.Fatalf("got\n%s", strings.Join(warnings, "\n"))
	}
}

func TestGraphQLErrors(t *testing.T) {
	_, err := ParseReviewThreadsResponse([]byte(`{"errors":[{"message":"a"},{"message":""},{"message":"b"}]}`), nil)
	if err == nil || err.Error() != "a\nb" {
		t.Fatalf("got %v", err)
	}
	_, err = ParseReviewThreadsResponse([]byte(`nope`), nil)
	if err == nil || err.Error() != "Invalid JSON returned from gh api graphql" {
		t.Fatalf("got %v", err)
	}
	page, err := ParseReviewThreadsResponse([]byte(`{"data":{"repository":null}}`), nil)
	if err != nil || len(page.Imports) != 0 || page.HasNextPage {
		t.Fatalf("got %+v %v", page, err)
	}
}

func TestParsePRURL(t *testing.T) {
	valid := map[string]PullRequest{
		"https://github.com/owner/repo/pull/123":              {"owner", "repo", "github.com", 123},
		"https://github.com/owner/repo/pull/123/files":        {"owner", "repo", "github.com", 123},
		"https://github.com/owner/repo/pull/123?tab=files":    {"owner", "repo", "github.com", 123},
		"https://github.com/my-org/my_repo/pull/7":            {"my-org", "my_repo", "github.com", 7},
		"https://git.company.io/team/project/pull/456#review": {"team", "project", "git.company.io", 456},
	}
	for url, want := range valid {
		if got, ok := ParsePRURL(url); !ok || got != want {
			t.Errorf("%s: %+v %v", url, got, ok)
		}
	}
	for _, url := range []string{"not-a-url", "", "https://github.com/owner/repo/issues/123", "https://github.com/owner/repo",
		"https://github.com/owner/repo/pull/abc", "https://github.com", "https://github.com/owner/repo/pull", "github.com/o/r/pull/1"} {
		if _, ok := ParsePRURL(url); ok {
			t.Errorf("accepted %q", url)
		}
	}
}

func stubGh(t *testing.T, fn func(args []string) ([]byte, error)) *[][]string {
	var calls [][]string
	orig := RunGh
	RunGh = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return fn(args)
	}
	t.Cleanup(func() { RunGh = orig })
	return &calls
}

func TestGetPRPatch(t *testing.T) {
	stubGh(t, func(args []string) ([]byte, error) {
		return nil, &GhError{Stderr: "could not find pull request\n", Err: errors.New("exit status 1")}
	})
	if _, err := GetPRPatch("12"); err == nil || err.Error() != "could not find pull request\nTry: gh auth login" {
		t.Fatalf("got %v", err)
	}
	stubGh(t, func(args []string) ([]byte, error) { return []byte("  \n"), nil })
	if _, err := GetPRPatch("12"); err == nil || err.Error() != "No diff content returned from gh pr diff\nTry: gh auth login" {
		t.Fatalf("got %v", err)
	}
	calls := stubGh(t, func(args []string) ([]byte, error) { return []byte("diff --git a/x b/x\n"), nil })
	if patch, err := GetPRPatch("12"); err != nil || patch != "diff --git a/x b/x\n" || !slices.Equal((*calls)[0], []string{"pr", "diff", "12"}) {
		t.Fatalf("got %q %v %v", patch, err, *calls)
	}
}

func TestGetPRCommentImportsPaginates(t *testing.T) {
	pages := [][]byte{
		response(t, true, "C1", thread(map[string]any{"id": "T1"})),
		response(t, false, nil, thread(map[string]any{"id": "T2", "line": 30})),
	}
	calls := stubGh(t, func(args []string) ([]byte, error) {
		page := pages[0]
		pages = pages[1:]
		return page, nil
	})
	imports, err := GetPRCommentImports("https://GHE.example.com/o/r/pull/5", nil)
	if err != nil || len(imports) != 2 {
		t.Fatalf("got %v %v", imports, err)
	}
	first, second := (*calls)[0], (*calls)[1]
	if !slices.Contains(first, "ghe.example.com") || !slices.Contains(first, "number=5") || slices.Contains(first, "endCursor=C1") {
		t.Fatalf("first call %v", first)
	}
	if second[len(second)-1] != "endCursor=C1" {
		t.Fatalf("second call %v", second)
	}

	if _, err := GetPRCommentImports("123", nil); err == nil || err.Error() != "Invalid GitHub PR URL" {
		t.Fatalf("got %v", err)
	}
	stubGh(t, func(args []string) ([]byte, error) { return response(t, true, nil), nil })
	if _, err := GetPRCommentImports("https://github.com/o/r/pull/1", nil); err == nil ||
		err.Error() != "GitHub GraphQL response indicated more pages without endCursor." {
		t.Fatalf("got %v", err)
	}
}
