// Package github loads pull request diffs and review comments via the gh CLI.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/tdryer/difit-go/internal/collate"
	"github.com/tdryer/difit-go/internal/types"
)

type PullRequest struct {
	Owner, Repo, Hostname string
	Number                int
}

// RunGh runs gh and returns its stdout. Tests replace it.
var RunGh = func(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, &GhError{Stderr: stderr.String(), Err: err}
	}
	return out, nil
}

// GhError is a failed gh invocation.
type GhError struct {
	Stderr string
	Err    error
}

func (e *GhError) Error() string { return e.Err.Error() }

func (e *GhError) Unwrap() error { return e.Err }

// formatGhError prefers gh's own stderr and adds a login hint.
func formatGhError(err error) error {
	message := err.Error()
	var ghErr *GhError
	if errors.As(err, &ghErr) {
		if stderr := strings.TrimSpace(ghErr.Stderr); stderr != "" {
			message = stderr
		}
	}
	return fmt.Errorf("%s\nTry: gh auth login", message)
}

// leadingInt mimics parseInt(s, 10).
func leadingInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r")
	end := 0
	if end < len(s) && (s[end] == '+' || s[end] == '-') {
		end++
	}
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == digits {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

// ParsePRURL extracts owner, repo and number from a pull request URL.
func ParsePRURL(raw string) (PullRequest, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Opaque != "" {
		return PullRequest{}, false
	}
	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 4 || parts[2] != "pull" {
		return PullRequest{}, false
	}
	n, ok := leadingInt(parts[3])
	if !ok {
		return PullRequest{}, false
	}
	return PullRequest{Owner: parts[0], Repo: parts[1], Number: n, Hostname: strings.ToLower(u.Hostname())}, true
}

// GetPRPatch returns the diff of a pull request (URL, number or branch).
func GetPRPatch(pr string) (string, error) {
	out, err := RunGh("pr", "diff", pr)
	if err == nil && strings.TrimSpace(string(out)) == "" {
		err = errors.New("No diff content returned from gh pr diff")
	}
	if err != nil {
		return "", formatGhError(err)
	}
	return string(out), nil
}

const reviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $endCursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $endCursor) {
        nodes {
          id
          isResolved
          isOutdated
          subjectType
          path
          diffSide
          startDiffSide
          line
          startLine
          originalLine
          originalStartLine
          comments(first: 100) {
            nodes {
              id
              body
              createdAt
              updatedAt
              author {
                login
              }
            }
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}
`

type reviewComment struct {
	ID        *string `json:"id"`
	Body      *string `json:"body"`
	CreatedAt *string `json:"createdAt"`
	UpdatedAt *string `json:"updatedAt"`
	Author    *struct {
		Login *string `json:"login"`
	} `json:"author"`
}

type reviewThread struct {
	ID                *string  `json:"id"`
	IsResolved        bool     `json:"isResolved"`
	IsOutdated        bool     `json:"isOutdated"`
	SubjectType       *string  `json:"subjectType"`
	Path              *string  `json:"path"`
	DiffSide          *string  `json:"diffSide"`
	StartDiffSide     *string  `json:"startDiffSide"`
	Line              *float64 `json:"line"`
	StartLine         *float64 `json:"startLine"`
	OriginalLine      *float64 `json:"originalLine"`
	OriginalStartLine *float64 `json:"originalStartLine"`
	Comments          *struct {
		Nodes []reviewComment `json:"nodes"`
	} `json:"comments"`
}

type graphqlResponse struct {
	Data *struct {
		Repository *struct {
			PullRequest *struct {
				ReviewThreads *struct {
					Nodes    []reviewThread `json:"nodes"`
					PageInfo *struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message *string `json:"message"`
	} `json:"errors"`
}

// Page is one page of review threads converted to comment imports.
type Page struct {
	Imports     []types.CommentImport
	HasNextPage bool
	EndCursor   string
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func positiveInt(v *float64) (int, bool) {
	if v == nil || *v <= 0 || *v != math.Trunc(*v) {
		return 0, false
	}
	return int(*v), true
}

type converter struct {
	warn func(string)
}

func (c converter) skip(t reviewThread, message string) {
	label := ""
	if t.ID != nil && *t.ID != "" {
		label = " " + *t.ID
	}
	c.warn(fmt.Sprintf("Warning: Skipping PR review thread%s: %s", label, message))
}

// sidePosition maps a GitHub line (and optional start line) on one side of
// the diff to a difit position.
func (c converter) sidePosition(t reviewThread, ghSide, side string, line, start *float64, missing string) (types.DiffCommentPosition, bool) {
	end, ok := positiveInt(line)
	if !ok {
		c.skip(t, ghSide+" thread is missing "+missing+".")
		return types.DiffCommentPosition{}, false
	}
	redundantStart := start != nil && *start == *line && t.StartDiffSide == nil
	multiLine := t.StartDiffSide != nil || (start != nil && !redundantStart)
	if !multiLine {
		return types.DiffCommentPosition{Side: side, Line: types.SingleLine(end)}, true
	}
	if deref(t.StartDiffSide) != ghSide {
		c.skip(t, ghSide+" thread has mismatched startDiffSide.")
		return types.DiffCommentPosition{}, false
	}
	first, ok := positiveInt(start)
	if !ok || first > end {
		c.skip(t, ghSide+" thread has an invalid multi-line range.")
		return types.DiffCommentPosition{}, false
	}
	return types.DiffCommentPosition{Side: side, Line: types.Range(first, end)}, true
}

func (c converter) position(t reviewThread) (types.DiffCommentPosition, bool) {
	switch deref(t.DiffSide) {
	case "RIGHT":
		return c.sidePosition(t, "RIGHT", "new", t.Line, t.StartLine, "line")
	case "LEFT":
		return c.sidePosition(t, "LEFT", "old", t.OriginalLine, t.OriginalStartLine, "originalLine")
	}
	c.skip(t, "Unsupported diffSide.")
	return types.DiffCommentPosition{}, false
}

func commentComplete(cm reviewComment) bool {
	return cm.ID != nil && cm.Body != nil && cm.CreatedAt != nil && cm.UpdatedAt != nil
}

func toImport(typ, filePath string, pos types.DiffCommentPosition, cm reviewComment) types.CommentImport {
	var author *string
	if cm.Author != nil && cm.Author.Login != nil {
		author = cm.Author.Login
	}
	return types.CommentImport{
		Type:      typ,
		ID:        cm.ID,
		FilePath:  filePath,
		Position:  pos,
		Body:      *cm.Body,
		Author:    author,
		CreatedAt: cm.CreatedAt,
		UpdatedAt: cm.UpdatedAt,
	}
}

func (c converter) threadImports(t reviewThread) []types.CommentImport {
	if t.IsResolved || t.IsOutdated || deref(t.SubjectType) != "LINE" {
		return nil
	}
	if t.Path == nil || strings.TrimSpace(*t.Path) == "" {
		c.skip(t, "Thread is missing path.")
		return nil
	}
	pos, ok := c.position(t)
	if !ok {
		return nil
	}
	var comments []reviewComment
	if t.Comments != nil {
		comments = slices.Clone(t.Comments.Nodes)
	}
	slices.SortStableFunc(comments, func(a, b reviewComment) int {
		return collate.Compare(deref(a.CreatedAt), deref(b.CreatedAt))
	})
	if len(comments) == 0 {
		c.skip(t, "Thread has no comments.")
		return nil
	}
	if !commentComplete(comments[0]) {
		c.skip(t, "Thread has a comment with missing fields.")
		return nil
	}
	imports := []types.CommentImport{toImport("thread", *t.Path, pos, comments[0])}
	for _, cm := range comments[1:] {
		if !commentComplete(cm) {
			c.skip(t, "Thread has a comment with missing fields.")
			continue
		}
		imports = append(imports, toImport("reply", *t.Path, pos, cm))
	}
	return imports
}

// ParseReviewThreadsResponse converts a GraphQL response page. Skipped
// threads are reported through warn.
func ParseReviewThreadsResponse(data []byte, warn func(string)) (Page, error) {
	var resp graphqlResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Page{}, errors.New("Invalid JSON returned from gh api graphql")
	}
	var messages []string
	for _, e := range resp.Errors {
		if e.Message != nil && *e.Message != "" {
			messages = append(messages, *e.Message)
		}
	}
	if len(messages) > 0 {
		return Page{}, errors.New(strings.Join(messages, "\n"))
	}

	page := Page{Imports: []types.CommentImport{}}
	if resp.Data == nil || resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil ||
		resp.Data.Repository.PullRequest.ReviewThreads == nil {
		return page, nil
	}
	threads := resp.Data.Repository.PullRequest.ReviewThreads
	c := converter{warn: warn}
	for _, t := range threads.Nodes {
		page.Imports = append(page.Imports, c.threadImports(t)...)
	}
	if threads.PageInfo != nil {
		page.HasNextPage = threads.PageInfo.HasNextPage
		page.EndCursor = deref(threads.PageInfo.EndCursor)
	}
	return page, nil
}

// GetPRCommentImports fetches unresolved inline review threads of a pull
// request given by URL.
func GetPRCommentImports(prURL string, warn func(string)) ([]types.CommentImport, error) {
	pr, ok := ParsePRURL(prURL)
	if !ok {
		return nil, errors.New("Invalid GitHub PR URL")
	}
	imports := []types.CommentImport{}
	cursor := ""
	for {
		args := []string{
			"api", "graphql",
			"--hostname", pr.Hostname,
			"-f", "query=" + reviewThreadsQuery,
			"-F", "owner=" + pr.Owner,
			"-F", "repo=" + pr.Repo,
			"-F", "number=" + strconv.Itoa(pr.Number),
		}
		if cursor != "" {
			args = append(args, "-F", "endCursor="+cursor)
		}
		out, err := RunGh(args...)
		if err != nil {
			return nil, formatGhError(err)
		}
		page, err := ParseReviewThreadsResponse(out, warn)
		if err != nil {
			return nil, err
		}
		imports = append(imports, page.Imports...)
		if !page.HasNextPage {
			return imports, nil
		}
		if page.EndCursor == "" {
			return nil, errors.New("GitHub GraphQL response indicated more pages without endCursor.")
		}
		cursor = page.EndCursor
	}
}
