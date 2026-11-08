// Package types mirrors the JSON shapes shared with the web client
// (web/src/types/diff.ts and watch.ts).
package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

type DiffLine struct {
	Type          string `json:"type"`
	Content       string `json:"content"`
	OldLineNumber *int   `json:"oldLineNumber,omitempty"`
	NewLineNumber *int   `json:"newLineNumber,omitempty"`
}

type DiffChunk struct {
	Header   string     `json:"header"`
	OldStart int        `json:"oldStart"`
	OldLines int        `json:"oldLines"`
	NewStart int        `json:"newStart"`
	NewLines int        `json:"newLines"`
	Lines    []DiffLine `json:"lines"`
}

type DiffFile struct {
	Path        string      `json:"path"`
	OldPath     string      `json:"oldPath,omitempty"`
	Status      string      `json:"status"`
	Additions   int         `json:"additions"`
	Deletions   int         `json:"deletions"`
	Chunks      []DiffChunk `json:"chunks"`
	IsGenerated bool        `json:"isGenerated"`
}

// BaseMode is "" (direct) or BaseModeMergeBase.
type BaseMode = string

const BaseModeMergeBase BaseMode = "merge-base"

type DiffSelection struct {
	BaseCommitish   string   `json:"baseCommitish"`
	TargetCommitish string   `json:"targetCommitish"`
	BaseMode        BaseMode `json:"baseMode,omitempty"`
}

// NewDiffSelection drops any base mode other than merge-base, matching
// createDiffSelection in the client.
func NewDiffSelection(base, target string, baseMode BaseMode) DiffSelection {
	if baseMode != BaseModeMergeBase {
		baseMode = ""
	}
	return DiffSelection{BaseCommitish: base, TargetCommitish: target, BaseMode: baseMode}
}

func normalizeBaseMode(m BaseMode) string {
	if m == "" {
		return "direct"
	}
	return m
}

// Key identifies a selection for caches and comment sessions.
func (s DiffSelection) Key() string {
	return s.BaseCommitish + ":" + s.TargetCommitish + ":" + normalizeBaseMode(s.BaseMode)
}

func (s DiffSelection) Equal(o DiffSelection) bool {
	return s.Key() == o.Key()
}

// MergeBaseTargetRef maps special targets to the commit they are based on.
func MergeBaseTargetRef(target string) string {
	if target == "." || target == "staged" || target == "working" {
		return "HEAD"
	}
	return target
}

type DiffResponse struct {
	Commit                   string          `json:"commit"`
	Files                    []DiffFile      `json:"files"`
	IsEmpty                  bool            `json:"isEmpty"`
	BaseCommitish            string          `json:"baseCommitish,omitempty"`
	TargetCommitish          string          `json:"targetCommitish,omitempty"`
	RequestedBaseCommitish   string          `json:"requestedBaseCommitish,omitempty"`
	RequestedTargetCommitish string          `json:"requestedTargetCommitish,omitempty"`
	RequestedBaseMode        BaseMode        `json:"requestedBaseMode,omitempty"`
	IgnoreWhitespace         bool            `json:"ignoreWhitespace"`
	OpenInEditorAvailable    bool            `json:"openInEditorAvailable"`
	ClearComments            bool            `json:"clearComments,omitempty"`
	RepositoryID             string          `json:"repositoryId,omitempty"`
	CommentImports           []CommentImport `json:"commentImports,omitempty"`
	CommentImportID          string          `json:"commentImportId,omitempty"`
}

type GeneratedStatusResponse struct {
	Path        string `json:"path"`
	Ref         string `json:"ref"`
	IsGenerated bool   `json:"isGenerated"`
	Source      string `json:"source"`
}

type RevisionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type BranchInfo struct {
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

type CommitInfo struct {
	Hash      string `json:"hash"`
	ShortHash string `json:"shortHash"`
	Message   string `json:"message"`
}

type RevisionsResponse struct {
	SpecialOptions      []RevisionOption `json:"specialOptions"`
	Branches            []BranchInfo     `json:"branches"`
	Commits             []CommitInfo     `json:"commits"`
	OriginDefaultBranch string           `json:"originDefaultBranch,omitempty"`
	ResolvedBase        string           `json:"resolvedBase,omitempty"`
	ResolvedTarget      string           `json:"resolvedTarget,omitempty"`
}

// LineRange is a single line (Start == End, encoded as a number) or an
// inclusive range (encoded as {"start","end"}).
type LineRange struct {
	Start, End int
	IsRange    bool
}

func SingleLine(n int) LineRange { return LineRange{Start: n, End: n} }

func Range(start, end int) LineRange { return LineRange{Start: start, End: end, IsRange: true} }

func (l LineRange) MarshalJSON() ([]byte, error) {
	if !l.IsRange {
		return json.Marshal(l.Start)
	}
	return json.Marshal(struct {
		Start int `json:"start"`
		End   int `json:"end"`
	}{l.Start, l.End})
}

func (l *LineRange) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		var r struct {
			Start int `json:"start"`
			End   int `json:"end"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		*l = Range(r.Start, r.End)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return errors.New("invalid line value")
	}
	*l = SingleLine(n)
	return nil
}

type DiffCommentPosition struct {
	Side string    `json:"side"`
	Line LineRange `json:"line"`
}

type CodeSnapshot struct {
	Content  string  `json:"content"`
	Language *string `json:"language,omitempty"`
}

type DiffCommentMessage struct {
	ID        string  `json:"id"`
	Body      string  `json:"body"`
	Author    *string `json:"author,omitempty"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
}

type DiffCommentThread struct {
	ID           string               `json:"id"`
	FilePath     string               `json:"filePath"`
	CreatedAt    string               `json:"createdAt"`
	UpdatedAt    string               `json:"updatedAt"`
	Position     DiffCommentPosition  `json:"position"`
	CodeSnapshot *CodeSnapshot        `json:"codeSnapshot,omitempty"`
	Messages     []DiffCommentMessage `json:"messages"`
}

// CommentImport is a thread or reply to merge into a review session.
// Field order matches serializeCommentImports so import IDs hash identically.
type CommentImport struct {
	Type         string              `json:"type"`
	ID           *string             `json:"id,omitempty"`
	FilePath     string              `json:"filePath"`
	Position     DiffCommentPosition `json:"position"`
	Body         string              `json:"body"`
	Author       *string             `json:"author,omitempty"`
	CreatedAt    *string             `json:"createdAt,omitempty"`
	UpdatedAt    *string             `json:"updatedAt,omitempty"`
	CodeSnapshot *CodeSnapshot       `json:"codeSnapshot,omitempty"`
}

// CommentThread is the flattened shape used for text output.
type CommentThread struct {
	ID          string
	File        string
	Line        LineRange
	Side        string
	CreatedAt   string
	UpdatedAt   string
	CodeContent *string
	Messages    []DiffCommentMessage
}

// DiffMode selects what the file watcher reacts to.
type DiffMode string

const (
	DiffModeDefault  DiffMode = "default"  // HEAD^ vs HEAD
	DiffModeWorking  DiffMode = "working"  // staged vs working
	DiffModeStaged   DiffMode = "staged"   // HEAD vs staged
	DiffModeDot      DiffMode = "dot"      // HEAD vs working (all changes)
	DiffModeSpecific DiffMode = "specific" // commit vs commit (no watching)
)

type WatchEvent struct {
	Type       string   `json:"type"`
	DiffMode   DiffMode `json:"diffMode,omitempty"`
	ChangeType string   `json:"changeType,omitempty"`
	Version    *int     `json:"version,omitempty"`
	Timestamp  string   `json:"timestamp"`
	Message    string   `json:"message,omitempty"`
}

// Now returns the current time formatted like JavaScript's toISOString.
func Now() string {
	return FormatTime(time.Now())
}

func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }
