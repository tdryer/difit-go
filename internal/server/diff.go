package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"difit-go/internal/gitdiff"
	"difit-go/internal/types"
)

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ignoreWhitespace := q.Get("ignoreWhitespace") == "true"

	s.mu.Lock()
	requested := selectionFromQuery(q, s.currentSelection)
	s.mu.Unlock()

	includeImports := len(s.opts.CommentImports) > 0 && (s.stdin || requested.Equal(s.opts.Selection))

	resp := s.initialDiff
	if !s.stdin {
		key := diffCacheKey(requested, ignoreWhitespace)
		s.mu.Lock()
		cached, ok := s.getCachedDiffLocked(key)
		s.mu.Unlock()
		if ok {
			resp = cached
		} else {
			parsed, err := s.parser.ParseDiff(requested, ignoreWhitespace, s.opts.ContextLines)
			if err != nil {
				s.logError("Error fetching diff:", err)
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			resp = parsed
			s.mu.Lock()
			s.setCachedDiffLocked(key, resp)
			clear(s.generatedCache)
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	s.currentSelection = requested
	s.currentCommentSelection = s.resolvedCommentSelection(resp, requested)
	s.mu.Unlock()

	orStdin := func(v string) string {
		if v == "" && s.stdin {
			return "stdin"
		}
		return v
	}
	out := resp
	out.BaseCommitish = orStdin(resp.BaseCommitish)
	out.TargetCommitish = orStdin(resp.TargetCommitish)
	if out.RequestedBaseCommitish == "" {
		out.RequestedBaseCommitish = orStdin(requested.BaseCommitish)
	}
	if out.RequestedTargetCommitish == "" {
		out.RequestedTargetCommitish = orStdin(requested.TargetCommitish)
	}
	if out.RequestedBaseMode == "" {
		out.RequestedBaseMode = requested.BaseMode
	}
	out.IgnoreWhitespace = ignoreWhitespace
	out.OpenInEditorAvailable = !s.stdin
	out.ClearComments = s.opts.ClearComments
	out.RepositoryID = s.repositoryID
	out.CommentImports = nil
	out.CommentImportID = ""
	if includeImports {
		out.CommentImports = s.opts.CommentImports
		out.CommentImportID = s.commentImportID
	}
	writeJSON(w, http.StatusOK, out)
}

// repoPath validates a client-supplied path, writing a 400 on failure.
func (s *Server) repoRelativePath(w http.ResponseWriter, path string) (string, bool) {
	normalized, err := gitdiff.NormalizeRepoRelativePath(s.repoPath, path)
	if err != nil {
		message := err.Error()
		if !errors.Is(err, gitdiff.ErrInvalidFilePath) && !errors.Is(err, gitdiff.ErrPathOutsideRepo) {
			message = gitdiff.ErrPathOutsideRepo.Error()
		}
		writeError(w, http.StatusBadRequest, message)
		return "", false
	}
	return normalized, true
}

func (s *Server) handleGeneratedStatus(w http.ResponseWriter, r *http.Request) {
	if s.stdin {
		writeError(w, http.StatusBadRequest, "Generated status is not available for stdin diff")
		return
	}
	path, ok := s.repoRelativePath(w, r.PathValue("path"))
	if !ok {
		return
	}

	ref := r.URL.Query().Get("ref")
	s.mu.Lock()
	if ref == "" {
		ref = s.currentSelection.TargetCommitish
	}
	if ref == "" {
		ref = "HEAD"
	}
	key := ref + ":" + path
	now := time.Now()
	cached, ok := s.generatedCache[key]
	s.mu.Unlock()
	if ok && cached.expiresAt.After(now) {
		writeJSON(w, http.StatusOK, cached.value)
		return
	}

	isGenerated, source := s.parser.GetGeneratedStatus(path, ref)
	resp := types.GeneratedStatusResponse{Path: path, Ref: ref, IsGenerated: isGenerated, Source: source}
	s.mu.Lock()
	s.generatedCache[key] = generatedCacheEntry{value: resp, expiresAt: now.Add(generatedStatusCacheTTL)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRevisions(w http.ResponseWriter, r *http.Request) {
	if s.stdin {
		writeError(w, http.StatusBadRequest, "Revision selection not available for stdin diff")
		return
	}
	s.mu.Lock()
	sel := s.currentSelection
	s.mu.Unlock()

	opts, err := s.parser.GetRevisionOptions(sel.BaseCommitish, sel.TargetCommitish)
	if err != nil {
		s.logError("Error fetching revisions:", err)
		writeError(w, http.StatusInternalServerError, "Failed to fetch revisions")
		return
	}
	writeJSON(w, http.StatusOK, types.RevisionsResponse{
		SpecialOptions: []types.RevisionOption{
			{Value: ".", Label: "All Uncommitted Changes"},
			{Value: "staged", Label: "Staging Area"},
			{Value: "working", Label: "Working Directory"},
		},
		Branches:            opts.Branches,
		Commits:             opts.Commits,
		OriginDefaultBranch: opts.OriginDefaultBranch,
		ResolvedBase:        opts.ResolvedBase,
		ResolvedTarget:      opts.ResolvedTarget,
	})
}

func (s *Server) handleLineCount(w http.ResponseWriter, r *http.Request) {
	if s.stdin {
		writeError(w, http.StatusNotFound, "Line count not available for stdin diff")
		return
	}
	path, ok := s.repoRelativePath(w, r.PathValue("path"))
	if !ok {
		return
	}
	q := r.URL.Query()
	oldPath := path
	if p := q.Get("oldPath"); p != "" {
		if oldPath, ok = s.repoRelativePath(w, p); !ok {
			return
		}
	}

	var result struct {
		OldLineCount *int `json:"oldLineCount,omitempty"`
		NewLineCount *int `json:"newLineCount,omitempty"`
	}
	if ref := q.Get("oldRef"); ref != "" {
		n, _ := s.parser.GetLineCount(oldPath, ref)
		result.OldLineCount = &n
	}
	if ref := q.Get("newRef"); ref != "" {
		n, _ := s.parser.GetLineCount(path, ref)
		result.NewLineCount = &n
	}
	writeJSON(w, http.StatusOK, result)
}

var imageContentTypes = map[string]string{
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"gif":  "image/gif",
	"bmp":  "image/bmp",
	"svg":  "image/svg+xml",
	"webp": "image/webp",
	"ico":  "image/x-icon",
	"tiff": "image/tiff",
	"tif":  "image/tiff",
	"avif": "image/avif",
	"heic": "image/heic",
	"heif": "image/heif",
}

func blobContentType(path string) string {
	ext := strings.ToLower(path[strings.LastIndex(path, ".")+1:])
	if ct, ok := imageContentTypes[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	if s.stdin {
		writeError(w, http.StatusNotFound, "Blob content not available for stdin diff")
		return
	}
	path, ok := s.repoRelativePath(w, r.PathValue("path"))
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "HEAD"
	}
	blob, err := s.parser.GetBlobContent(path, ref)
	if err != nil {
		s.logError("Error fetching blob:", err)
		writeError(w, http.StatusNotFound, "File not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", blobContentType(path))
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	w.Write(blob)
}
