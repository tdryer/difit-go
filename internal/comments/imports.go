// Package comments ports the comment import/merge logic and the text output
// format shared with the web client (web/src/utils/commentImports.ts and
// commentFormatting.ts).
package comments

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"difit-go/internal/collate"
	"difit-go/internal/types"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// CreateID returns a random 16-character identifier.
func CreateID() string {
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(idAlphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = idAlphabet[n.Int64()]
	}
	return string(b)
}

// MergeResult holds merged threads plus warnings about skipped imports.
type MergeResult struct {
	Threads  []types.DiffCommentThread
	Warnings []string
}

func fieldError(field string) error {
	return fmt.Errorf("Invalid comment import field: %s", field)
}

// isoDatePattern accepts the ISO 8601 forms Date.parse understands.
var isoDatePattern = regexp.MustCompile(`^[+-]?\d{4,6}(-\d{2}(-\d{2})?)?(T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$`)

func isValidISOTimestamp(value string) bool {
	if !isoDatePattern.MatchString(value) {
		_, err := time.Parse(time.RFC1123, value)
		return err == nil
	}
	// Reject out-of-range components such as month 13.
	layouts := []string{
		"2006", "2006-01", "2006-01-02", "2006-01-02T15:04", "2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05.999999999Z0700",
	}
	for _, layout := range layouts {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}

// decodeAny parses JSON keeping numbers as json.Number.
func decodeAny(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected trailing data")
	}
	return v, nil
}

func positiveInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || f <= 0 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

func normalizeTimestamp(obj map[string]any, field string) (*string, error) {
	v, present := obj[field]
	if !present {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" || !isValidISOTimestamp(s) {
		return nil, fieldError(field)
	}
	return &s, nil
}

func normalizeLineRange(v any) (types.LineRange, error) {
	if n, ok := positiveInt(v); ok {
		return types.SingleLine(n), nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return types.LineRange{}, fieldError("position.line")
	}
	start, okStart := positiveInt(obj["start"])
	end, okEnd := positiveInt(obj["end"])
	if !okStart || !okEnd || start > end {
		return types.LineRange{}, fieldError("position.line")
	}
	return types.Range(start, end), nil
}

func normalizePosition(v any) (types.DiffCommentPosition, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return types.DiffCommentPosition{}, fieldError("position")
	}
	side, _ := obj["side"].(string)
	if side != "old" && side != "new" {
		return types.DiffCommentPosition{}, fieldError("position.side")
	}
	line, err := normalizeLineRange(obj["line"])
	if err != nil {
		return types.DiffCommentPosition{}, err
	}
	return types.DiffCommentPosition{Side: side, Line: line}, nil
}

func normalizeCodeSnapshot(obj map[string]any) (*types.CodeSnapshot, error) {
	v, present := obj["codeSnapshot"]
	if !present {
		return nil, nil
	}
	snap, ok := v.(map[string]any)
	if !ok {
		return nil, fieldError("codeSnapshot")
	}
	content, ok := snap["content"].(string)
	if !ok {
		return nil, fieldError("codeSnapshot")
	}
	result := &types.CodeSnapshot{Content: content}
	if lang, present := snap["language"]; present {
		s, ok := lang.(string)
		if !ok {
			return nil, fieldError("codeSnapshot.language")
		}
		result.Language = &s
	}
	return result, nil
}

func normalizeOptionalString(obj map[string]any, field string) (*string, error) {
	v, present := obj[field]
	if !present {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fieldError(field)
	}
	return &s, nil
}

func normalizeEntry(v any) (types.CommentImport, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return types.CommentImport{}, errors.New("Comment import must be an object")
	}
	typ, _ := obj["type"].(string)
	if typ != "thread" && typ != "reply" {
		return types.CommentImport{}, fieldError("type")
	}
	filePath, ok := obj["filePath"].(string)
	if !ok || strings.TrimSpace(filePath) == "" {
		return types.CommentImport{}, fieldError("filePath")
	}
	createdAt, err := normalizeTimestamp(obj, "createdAt")
	if err != nil {
		return types.CommentImport{}, err
	}
	updatedAt, err := normalizeTimestamp(obj, "updatedAt")
	if err != nil {
		return types.CommentImport{}, err
	}
	id, err := normalizeOptionalString(obj, "id")
	if err != nil {
		return types.CommentImport{}, err
	}
	position, err := normalizePosition(obj["position"])
	if err != nil {
		return types.CommentImport{}, err
	}
	body, ok := obj["body"].(string)
	if !ok || strings.TrimSpace(body) == "" {
		return types.CommentImport{}, fieldError("body")
	}
	author, err := normalizeOptionalString(obj, "author")
	if err != nil {
		return types.CommentImport{}, err
	}
	snapshot, err := normalizeCodeSnapshot(obj)
	if err != nil {
		return types.CommentImport{}, err
	}
	return types.CommentImport{
		Type:         typ,
		ID:           id,
		FilePath:     filePath,
		Position:     position,
		Body:         strings.TrimSpace(body),
		Author:       author,
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
		CodeSnapshot: snapshot,
	}, nil
}

// NormalizeImports validates a decoded JSON object or array of comment imports.
func NormalizeImports(v any) ([]types.CommentImport, error) {
	list, isList := v.([]any)
	if !isList {
		list = []any{v}
	}
	result := make([]types.CommentImport, 0, len(list))
	for _, entry := range list {
		ci, err := normalizeEntry(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, ci)
	}
	return result, nil
}

// ParseImportsJSON validates raw JSON comment imports.
func ParseImportsJSON(data []byte) ([]types.CommentImport, error) {
	v, err := decodeAny(data)
	if err != nil {
		return nil, err
	}
	return NormalizeImports(v)
}

// ParseImportValue parses a --comment value.
func ParseImportValue(value string) ([]types.CommentImport, error) {
	v, err := decodeAny([]byte(value))
	if err != nil {
		return nil, errors.New("Invalid --comment JSON")
	}
	return NormalizeImports(v)
}

// MarshalJSON encodes like JSON.stringify: no HTML escaping, no trailing newline.
func MarshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// SerializeImports produces the canonical JSON used to identify an import batch.
func SerializeImports(imports []types.CommentImport) string {
	if imports == nil {
		imports = []types.CommentImport{}
	}
	data, err := MarshalJSON(imports)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// ImportID returns the SHA-256 of the serialized imports.
func ImportID(imports []types.CommentImport) string {
	sum := sha256.Sum256([]byte(SerializeImports(imports)))
	return hex.EncodeToString(sum[:])
}

func positionKey(p types.DiffCommentPosition) string {
	if !p.Line.IsRange {
		return fmt.Sprintf("%s:%d", p.Side, p.Line.Start)
	}
	return fmt.Sprintf("%s:%d-%d", p.Side, p.Line.Start, p.Line.End)
}

func positionsMatch(a, b types.DiffCommentPosition) bool {
	return positionKey(a) == positionKey(b)
}

func normalizeAuthor(author *string) string {
	if author == nil {
		return ""
	}
	return strings.TrimSpace(*author)
}

func messageMatchesImport(m types.DiffCommentMessage, ci types.CommentImport) bool {
	if ci.ID != nil && *ci.ID != "" && m.ID == *ci.ID {
		return true
	}
	if normalizeAuthor(m.Author) != normalizeAuthor(ci.Author) {
		return false
	}
	if m.Body != ci.Body {
		return false
	}
	if ci.CreatedAt != nil && *ci.CreatedAt != "" && m.CreatedAt != *ci.CreatedAt {
		return false
	}
	return true
}

func rootThreadMatchesImport(t types.DiffCommentThread, ci types.CommentImport) bool {
	if ci.ID != nil && *ci.ID != "" && t.ID == *ci.ID {
		return true
	}
	if t.FilePath != ci.FilePath || !positionsMatch(t.Position, ci.Position) {
		return false
	}
	if len(t.Messages) == 0 {
		return false
	}
	return messageMatchesImport(t.Messages[0], ci)
}

func importTimestamps(ci types.CommentImport, now string) (string, string) {
	createdAt := now
	if ci.CreatedAt != nil {
		createdAt = *ci.CreatedAt
	}
	updatedAt := createdAt
	if ci.UpdatedAt != nil {
		updatedAt = *ci.UpdatedAt
	}
	return createdAt, updatedAt
}

func maxTimestamp(a, b string) string {
	if collate.Compare(a, b) >= 0 {
		return a
	}
	return b
}

func createImportedThread(ci types.CommentImport, now string) types.DiffCommentThread {
	createdAt, updatedAt := importTimestamps(ci, now)
	threadID := CreateID()
	if ci.ID != nil {
		threadID = *ci.ID
	}
	return types.DiffCommentThread{
		ID:           threadID,
		FilePath:     ci.FilePath,
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
		Position:     ci.Position,
		CodeSnapshot: cloneSnapshot(ci.CodeSnapshot),
		Messages: []types.DiffCommentMessage{{
			ID:        threadID,
			Body:      ci.Body,
			Author:    ci.Author,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
		}},
	}
}

func createImportedReply(ci types.CommentImport, now string) types.DiffCommentMessage {
	createdAt, updatedAt := importTimestamps(ci, now)
	id := CreateID()
	if ci.ID != nil {
		id = *ci.ID
	}
	return types.DiffCommentMessage{ID: id, Body: ci.Body, Author: ci.Author, CreatedAt: createdAt, UpdatedAt: updatedAt}
}

func cloneSnapshot(s *types.CodeSnapshot) *types.CodeSnapshot {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

func cloneThread(t types.DiffCommentThread) types.DiffCommentThread {
	t.CodeSnapshot = cloneSnapshot(t.CodeSnapshot)
	t.Messages = slices.Clone(t.Messages)
	if t.Messages == nil {
		t.Messages = []types.DiffCommentMessage{}
	}
	return t
}

func cloneThreads(threads []types.DiffCommentThread) []types.DiffCommentThread {
	out := make([]types.DiffCommentThread, len(threads))
	for i, t := range threads {
		out[i] = cloneThread(t)
	}
	return out
}

func messagesMatch(a, b types.DiffCommentMessage) bool {
	if a.ID == b.ID {
		return true
	}
	if normalizeAuthor(a.Author) != normalizeAuthor(b.Author) {
		return false
	}
	return a.Body == b.Body && a.CreatedAt == b.CreatedAt
}

func threadsMatch(a, b types.DiffCommentThread) bool {
	if a.ID == b.ID {
		return true
	}
	if a.FilePath != b.FilePath || !positionsMatch(a.Position, b.Position) {
		return false
	}
	if len(a.Messages) == 0 || len(b.Messages) == 0 {
		return false
	}
	return messagesMatch(a.Messages[0], b.Messages[0])
}

func sortReplies(a, b types.DiffCommentMessage) int {
	if c := collate.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
		return c
	}
	return collate.Compare(a.ID, b.ID)
}

func pickNewerMessage(existing, incoming types.DiffCommentMessage) types.DiffCommentMessage {
	if collate.Compare(incoming.UpdatedAt, existing.UpdatedAt) >= 0 {
		return incoming
	}
	return existing
}

func mergeThread(existing, incoming types.DiffCommentThread) types.DiffCommentThread {
	merged := slices.Clone(existing.Messages)
	for _, msg := range incoming.Messages {
		idx := slices.IndexFunc(merged, func(m types.DiffCommentMessage) bool { return messagesMatch(m, msg) })
		if idx >= 0 {
			merged[idx] = pickNewerMessage(merged[idx], msg)
			continue
		}
		merged = append(merged, msg)
	}
	ordered := merged
	if len(ordered) > 1 {
		slices.SortStableFunc(ordered[1:], sortReplies)
	}
	if ordered == nil {
		ordered = []types.DiffCommentMessage{}
	}

	updatedAt := maxTimestamp(existing.UpdatedAt, incoming.UpdatedAt)
	for _, m := range ordered {
		updatedAt = maxTimestamp(updatedAt, m.UpdatedAt)
	}
	createdAt := incoming.CreatedAt
	if collate.Compare(existing.CreatedAt, incoming.CreatedAt) <= 0 {
		createdAt = existing.CreatedAt
	}
	snapshot := incoming.CodeSnapshot
	if snapshot == nil {
		snapshot = existing.CodeSnapshot
	}

	result := cloneThread(existing)
	result.CreatedAt = createdAt
	result.UpdatedAt = updatedAt
	result.CodeSnapshot = cloneSnapshot(snapshot)
	result.Messages = ordered
	return result
}

// MergeThreads merges incoming threads into existing ones, matching threads
// and messages by ID or by author/body/timestamp.
func MergeThreads(existing, incoming []types.DiffCommentThread) MergeResult {
	threads := cloneThreads(existing)
	for _, in := range incoming {
		idx := slices.IndexFunc(threads, func(t types.DiffCommentThread) bool { return threadsMatch(t, in) })
		if idx < 0 {
			threads = append(threads, cloneThread(in))
			continue
		}
		threads[idx] = mergeThread(threads[idx], in)
	}
	return MergeResult{Threads: threads, Warnings: []string{}}
}

// MergeImports applies comment imports to existing threads, skipping duplicates.
func MergeImports(existing []types.DiffCommentThread, imports []types.CommentImport) MergeResult {
	threads := cloneThreads(existing)
	warnings := []string{}

	for _, ci := range imports {
		now := types.Now()
		if ci.Type == "thread" {
			if slices.ContainsFunc(threads, func(t types.DiffCommentThread) bool { return rootThreadMatchesImport(t, ci) }) {
				continue
			}
			threads = append(threads, createImportedThread(ci, now))
			continue
		}

		target := -1
		for i, t := range threads {
			if t.FilePath != ci.FilePath || !positionsMatch(t.Position, ci.Position) {
				continue
			}
			// Newest thread wins; earlier threads win ties (stable sort).
			if target < 0 || collate.Compare(t.UpdatedAt, threads[target].UpdatedAt) > 0 {
				target = i
			}
		}
		if target < 0 {
			warnings = append(warnings, fmt.Sprintf(
				"Skipped reply import for %s:%s because no matching thread was found.", ci.FilePath, positionKey(ci.Position)))
			continue
		}

		t := &threads[target]
		if slices.ContainsFunc(t.Messages, func(m types.DiffCommentMessage) bool { return messageMatchesImport(m, ci) }) {
			continue
		}
		reply := createImportedReply(ci, now)
		t.Messages = append(t.Messages, reply)
		t.UpdatedAt = maxTimestamp(t.UpdatedAt, reply.UpdatedAt)
	}

	return MergeResult{Threads: threads, Warnings: warnings}
}
