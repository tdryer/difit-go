package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/tdryer/difit-go/internal/comments"
	"github.com/tdryer/difit-go/internal/types"
)

func (s *Server) sessionLocked(sel types.DiffSelection) *commentSession {
	key := sel.Key()
	session, ok := s.commentSessions[key]
	if !ok {
		session = &commentSession{threads: []types.DiffCommentThread{}}
		s.commentSessions[key] = session
	}
	return session
}

// updateSessionLocked stores threads and, if they changed, bumps the version
// and notifies clients.
func (s *Server) updateSessionLocked(sel types.DiffSelection, threads []types.DiffCommentThread) bool {
	session := s.sessionLocked(sel)
	if threads == nil {
		threads = []types.DiffCommentThread{}
	}
	previous, _ := comments.MarshalJSON(session.threads)
	next, _ := comments.MarshalJSON(threads)
	session.threads = threads
	if bytes.Equal(previous, next) {
		return false
	}
	session.version++
	s.watcher.Broadcast(types.WatchEvent{
		Type:      "commentsChanged",
		Version:   types.Ptr(session.version),
		Timestamp: types.Now(),
	})
	return true
}

func (s *Server) commentSelection(r *http.Request) types.DiffSelection {
	return selectionFromQuery(r.URL.Query(), s.currentCommentSelection)
}

// normalizeLine accepts a positive integer or a [start, end] pair, falling
// back to line 1.
func normalizeLine(v any) types.LineRange {
	if pair, ok := v.([]any); ok && len(pair) == 2 {
		start, okStart := positiveInt(pair[0])
		end, okEnd := positiveInt(pair[1])
		if okStart && okEnd && start <= end {
			return types.Range(start, end)
		}
	}
	if n, ok := positiveInt(v); ok {
		return types.SingleLine(n)
	}
	return types.SingleLine(1)
}

func positiveInt(v any) (int, bool) {
	num, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := num.Float64()
	if err != nil || f <= 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

func stringField(obj map[string]any, key string) string {
	v, _ := obj[key].(string)
	return v
}

func optionalString(obj map[string]any, key string) *string {
	if v, ok := obj[key].(string); ok {
		return &v
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func sideOf(obj map[string]any) string {
	if side, ok := obj["side"].(string); ok {
		return side
	}
	return "new"
}

func fileOf(obj map[string]any) string {
	return firstNonEmpty(stringField(obj, "file"), "<unknown file>")
}

func snapshotOf(obj map[string]any) *types.CodeSnapshot {
	if code, ok := obj["codeContent"].(string); ok {
		return &types.CodeSnapshot{Content: code}
	}
	return nil
}

// fallbackID derives a stable ID from the entry's JSON, like the original's
// sha256(JSON.stringify(entry)).slice(0, 12).
func fallbackID(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		compact.Write(raw)
	}
	return sha256Hex(compact.String())[:12]
}

// normalizeThreadPayload converts a client CommentThread (flattened shape)
// to a DiffCommentThread. Entries already in DiffCommentThread shape are kept.
func normalizeThreadPayload(raw json.RawMessage, now string) (types.DiffCommentThread, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return types.DiffCommentThread{}, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return types.DiffCommentThread{}, fmt.Errorf("thread must be an object")
	}
	if _, hasPath := obj["filePath"]; hasPath {
		if _, hasPos := obj["position"]; hasPos {
			var t types.DiffCommentThread
			if err := json.Unmarshal(raw, &t); err != nil {
				return t, err
			}
			if t.Messages == nil {
				t.Messages = []types.DiffCommentMessage{}
			}
			return t, nil
		}
	}

	id := stringField(obj, "id")
	if id == "" {
		id = fallbackID(raw)
	}
	threadCreated, threadUpdated := stringField(obj, "createdAt"), stringField(obj, "updatedAt")

	var messages []types.DiffCommentMessage
	if list, ok := obj["messages"].([]any); ok && len(list) > 0 {
		for i, entry := range list {
			m, _ := entry.(map[string]any)
			created := firstNonEmpty(stringField(m, "createdAt"), threadCreated, now)
			messages = append(messages, types.DiffCommentMessage{
				ID:        firstNonEmpty(stringField(m, "id"), fmt.Sprintf("%s:%d", id, i)),
				Body:      stringField(m, "body"),
				Author:    optionalString(m, "author"),
				CreatedAt: created,
				UpdatedAt: firstNonEmpty(stringField(m, "updatedAt"), stringField(m, "createdAt"), threadUpdated, now),
			})
		}
	} else {
		messages = []types.DiffCommentMessage{{
			ID:        id,
			CreatedAt: firstNonEmpty(threadCreated, now),
			UpdatedAt: firstNonEmpty(threadUpdated, threadCreated, now),
		}}
	}

	return types.DiffCommentThread{
		ID:        id,
		FilePath:  fileOf(obj),
		CreatedAt: firstNonEmpty(threadCreated, messages[0].CreatedAt, now),
		UpdatedAt: firstNonEmpty(threadUpdated, messages[len(messages)-1].UpdatedAt, threadCreated, now),
		Position: types.DiffCommentPosition{
			Side: sideOf(obj),
			Line: normalizeLine(obj["line"]),
		},
		CodeSnapshot: snapshotOf(obj),
		Messages:     messages,
	}, nil
}

// normalizeLegacyComment converts the pre-thread Comment shape.
func normalizeLegacyComment(raw json.RawMessage, now string) (types.DiffCommentThread, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return types.DiffCommentThread{}, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return types.DiffCommentThread{}, fmt.Errorf("comment must be an object")
	}
	timestamp := now
	if ts, ok := obj["timestamp"].(string); ok {
		timestamp = ts
	}
	id := stringField(obj, "id")
	if id == "" {
		id = fallbackID(raw)
	}
	return types.DiffCommentThread{
		ID:        id,
		FilePath:  fileOf(obj),
		CreatedAt: timestamp,
		UpdatedAt: timestamp,
		Position: types.DiffCommentPosition{
			Side: sideOf(obj),
			Line: normalizeLine(obj["line"]),
		},
		CodeSnapshot: snapshotOf(obj),
		Messages: []types.DiffCommentMessage{{
			ID:        id,
			Body:      stringField(obj, "body"),
			Author:    optionalString(obj, "author"),
			CreatedAt: timestamp,
			UpdatedAt: timestamp,
		}},
	}, nil
}

type commentsPayload struct {
	threads     []types.DiffCommentThread
	baseVersion *int
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func parseCommentsPayload(body []byte) (commentsPayload, error) {
	var result commentsPayload
	v, err := decodeJSON(body)
	if err != nil {
		return result, err
	}
	if v == nil {
		return result, fmt.Errorf("payload is null")
	}
	result.threads = []types.DiffCommentThread{}
	if _, ok := v.(map[string]any); !ok {
		return result, nil
	}

	var payload struct {
		Threads     json.RawMessage `json:"threads"`
		Comments    json.RawMessage `json:"comments"`
		BaseVersion json.RawMessage `json:"baseVersion"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return result, err
	}
	now := types.Now()
	var entries []json.RawMessage
	normalize := normalizeThreadPayload
	if isJSONArray(payload.Threads) {
		err = json.Unmarshal(payload.Threads, &entries)
	} else if isJSONArray(payload.Comments) {
		normalize = normalizeLegacyComment
		err = json.Unmarshal(payload.Comments, &entries)
	}
	if err != nil {
		return result, err
	}
	for _, raw := range entries {
		t, err := normalize(raw, now)
		if err != nil {
			return result, err
		}
		result.threads = append(result.threads, t)
	}

	if len(payload.BaseVersion) > 0 {
		if n, err := decodeJSON(payload.BaseVersion); err == nil {
			if num, ok := n.(json.Number); ok {
				if f, err := num.Float64(); err == nil && f >= 0 && f == math.Trunc(f) && f <= math.MaxInt32 {
					result.baseVersion = types.Ptr(int(f))
				}
			}
		}
	}
	return result, nil
}

func (s *Server) handlePostComments(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	var payload commentsPayload
	if err == nil {
		payload, err = parseCommentsPayload(body)
	}
	if err != nil {
		s.logError("Error parsing comments:", err)
		writeError(w, http.StatusBadRequest, "Invalid comment data")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sel := s.commentSelection(r)
	session := s.sessionLocked(sel)

	// A stale baseVersion means another writer (e.g. an agent) changed
	// comments since the client's last read, so merge rather than overwrite.
	// A matching or absent version replaces.
	stale := payload.baseVersion != nil && *payload.baseVersion != session.version
	threads := payload.threads
	if stale {
		threads = comments.MergeThreads(session.threads, payload.threads).Threads
	}
	s.updateSessionLocked(sel, threads)

	writeJSON(w, http.StatusOK, struct {
		Success bool                      `json:"success"`
		Merged  bool                      `json:"merged"`
		Version int                       `json:"version"`
		Threads []types.DiffCommentThread `json:"threads"`
	}{true, stale, session.version, session.threads})
}

func (s *Server) handleCommentImports(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	var imports []types.CommentImport
	if err == nil {
		imports, err = comments.ParseImportsJSON(body)
	}
	if err != nil {
		s.logError("Error parsing comment imports:", err)
		writeError(w, http.StatusBadRequest, "Invalid comment import data")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sel := s.commentSelection(r)
	merged := comments.MergeImports(s.sessionLocked(sel).threads, imports)
	changed := s.updateSessionLocked(sel, merged.Threads)
	warnings := merged.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Success  bool     `json:"success"`
		Changed  bool     `json:"changed"`
		Count    int      `json:"count"`
		ImportID string   `json:"importId"`
		Warnings []string `json:"warnings"`
	}{true, changed, len(imports), comments.ImportID(imports), warnings})
}

func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")

	s.mu.Lock()
	defer s.mu.Unlock()
	sel := s.commentSelection(r)
	session := s.sessionLocked(sel)
	next := make([]types.DiffCommentThread, 0, len(session.threads))
	for _, t := range session.threads {
		if t.ID != threadID {
			next = append(next, t)
		}
	}
	if len(next) == len(session.threads) {
		writeError(w, http.StatusNotFound, "Thread not found: "+threadID)
		return
	}
	s.updateSessionLocked(sel, next)
	writeJSON(w, http.StatusOK, struct {
		Success  bool   `json:"success"`
		ThreadID string `json:"threadId"`
		Version  int    `json:"version"`
	}{true, threadID, session.version})
}

func (s *Server) handleCommentsJSON(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessionLocked(s.commentSelection(r))
	writeJSON(w, http.StatusOK, struct {
		Version int                       `json:"version"`
		Threads []types.DiffCommentThread `json:"threads"`
	}{session.version, session.threads})
}

func (s *Server) handleCommentsOutput(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	session := s.sessionLocked(s.commentSelection(r))
	output := ""
	if len(session.threads) > 0 {
		output = comments.FormatOutput(session.threads)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(output))
}
