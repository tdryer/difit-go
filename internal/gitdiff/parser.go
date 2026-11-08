package gitdiff

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"difit-go/internal/collate"
	"difit-go/internal/generated"
	"difit-go/internal/types"
)

const (
	resolvedCommitCacheTTL   = 5 * time.Second
	generatedHeaderScanBytes = 4 * 1024
	gitattributesCheckChunk  = 200
	maxBlobBytes             = 10 * 1024 * 1024
	generatedHeaderLineLimit = 20
)

var (
	ErrInvalidFilePath   = errors.New("Invalid file path")
	ErrPathOutsideRepo   = errors.New("File path outside repository")
	originHeadRefPattern = regexp.MustCompile(`refs/remotes/origin/(.+)`)
)

// Parser produces diff data for a repository.
type Parser struct {
	repoPath string
	git      Git

	mu             sync.Mutex
	resolvedCommit map[string]cachedValue
}

type cachedValue struct {
	value     string
	expiresAt time.Time
}

func NewParser(repoPath string) *Parser {
	return &Parser{repoPath: repoPath, git: Git{Dir: repoPath}, resolvedCommit: map[string]cachedValue{}}
}

// Git returns the runner bound to the repository.
func (p *Parser) Git() Git { return p.git }

// NormalizeRepoRelativePath rejects absolute paths and parent traversal and
// converts backslashes to slashes.
func NormalizeRepoRelativePath(repoPath, path string) (string, error) {
	if path == "" {
		return "", ErrInvalidFilePath
	}
	normalized := strings.ReplaceAll(path, `\`, "/")
	if filepath.IsAbs(path) || strings.HasPrefix(normalized, "/") || slices.Contains(strings.Split(normalized, "/"), "..") {
		return "", ErrPathOutsideRepo
	}
	repo, err := filepath.Abs(repoPath)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(repo, filepath.FromSlash(normalized))
	if resolved != repo && !strings.HasPrefix(resolved, repo+string(filepath.Separator)) {
		return "", ErrPathOutsideRepo
	}
	return normalized, nil
}

func (p *Parser) resolveBaseCommitish(sel types.DiffSelection) (string, error) {
	if sel.BaseMode != types.BaseModeMergeBase {
		return sel.BaseCommitish, nil
	}
	out, err := p.git.RunString("merge-base", types.MergeBaseTargetRef(sel.TargetCommitish), sel.BaseCommitish)
	return strings.TrimSpace(out), err
}

// ParseDiff runs git diff for a selection. contextLines < 0 means git's default.
func (p *Parser) ParseDiff(sel types.DiffSelection, ignoreWhitespace bool, contextLines int) (types.DiffResponse, error) {
	resp, err := p.parseDiff(sel, ignoreWhitespace, contextLines)
	if err != nil {
		return types.DiffResponse{}, fmt.Errorf("Failed to parse diff for %s vs %s: %w", sel.TargetCommitish, sel.BaseCommitish, err)
	}
	return resp, nil
}

func (p *Parser) parseDiff(sel types.DiffSelection, ignoreWhitespace bool, contextLines int) (types.DiffResponse, error) {
	target, base := sel.TargetCommitish, sel.BaseCommitish
	if err := ValidateDiffArguments(target, base, true); err != nil {
		return types.DiffResponse{}, err
	}
	effectiveBase, err := p.resolveBaseCommitish(sel)
	if err != nil {
		return types.DiffResponse{}, err
	}

	var commit string
	var args []string
	resolvedBase, resolvedTarget, attributesRef := effectiveBase, target, target

	switch target {
	case "working":
		commit = "Working Directory (unstaged changes)"
	case "staged", ".":
		baseHash, err := p.git.RevParse(effectiveBase)
		if err != nil {
			return types.DiffResponse{}, err
		}
		resolvedBase = ShortHash(baseHash)
		if target == "staged" {
			commit = resolvedBase + " vs Staging Area (staged changes)"
			args = []string{"--cached", effectiveBase}
		} else {
			commit = resolvedBase + " vs Working Directory (all uncommitted changes)"
			args = []string{effectiveBase}
		}
	default:
		targetHash, err := p.git.RevParse(target)
		if err != nil {
			return types.DiffResponse{}, err
		}
		baseHash, err := p.git.RevParse(effectiveBase)
		if err != nil {
			return types.DiffResponse{}, err
		}
		resolvedBase, resolvedTarget = ShortHash(baseHash), ShortHash(targetHash)
		commit = resolvedBase + "..." + resolvedTarget
		attributesRef = targetHash
		args = []string{baseHash, targetHash}
	}

	if ignoreWhitespace {
		args = append(args, "-w")
	}
	if contextLines >= 0 {
		args = append(args, fmt.Sprintf("-U%d", contextLines))
	}
	// Ignore external diff tools so the output format is predictable.
	args = append(args, "--no-ext-diff", "--color=never")

	raw, err := p.git.RunString(append([]string{"diff"}, args...)...)
	if err != nil {
		return types.DiffResponse{}, err
	}
	files := p.markGitattributesGenerated(ParseUnifiedDiff(raw), attributesRef)

	resp := types.DiffResponse{
		Commit:                   commit,
		Files:                    files,
		IsEmpty:                  len(files) == 0,
		BaseCommitish:            resolvedBase,
		TargetCommitish:          resolvedTarget,
		RequestedBaseCommitish:   base,
		RequestedTargetCommitish: target,
	}
	if sel.BaseMode == types.BaseModeMergeBase {
		resp.RequestedBaseMode = types.BaseModeMergeBase
	}
	return resp, nil
}

// ParseStdinDiff parses a diff supplied on stdin or from a PR.
func ParseStdinDiff(text string) types.DiffResponse {
	files := ParseUnifiedDiff(text)
	return types.DiffResponse{Commit: "stdin diff", Files: files, IsEmpty: len(files) == 0}
}

func gitattributesSourceArgs(ref string) []string {
	switch ref {
	case "working", ".":
		return nil
	case "staged":
		return []string{"--cached"}
	}
	return []string{"--source", ref}
}

func (p *Parser) gitattributesGeneratedPaths(paths []string, ref string) map[string]bool {
	result := map[string]bool{}
	for i := 0; i < len(paths); i += gitattributesCheckChunk {
		chunk := paths[i:min(i+gitattributesCheckChunk, len(paths))]
		args := append([]string{"check-attr", "-z"}, gitattributesSourceArgs(ref)...)
		args = append(args, "linguist-generated", "--")
		args = append(args, chunk...)
		out, err := p.git.RunString(args...)
		if err != nil {
			return map[string]bool{}
		}
		fields := strings.Split(out, "\x00")
		for j := 0; j+2 < len(fields); j += 3 {
			if fields[j+1] == "linguist-generated" && fields[j+2] == "true" {
				result[fields[j]] = true
			}
		}
	}
	return result
}

func (p *Parser) markGitattributesGenerated(files []types.DiffFile, ref string) []types.DiffFile {
	var candidates []string
	for _, f := range files {
		if !f.IsGenerated {
			candidates = append(candidates, f.Path)
		}
	}
	if len(candidates) == 0 {
		return files
	}
	generatedPaths := p.gitattributesGeneratedPaths(candidates, ref)
	for i := range files {
		if generatedPaths[files[i].Path] {
			files[i].IsGenerated = true
		}
	}
	return files
}

// ValidateCommit checks that a commit-ish exists (or that we're in a repo,
// for the special working/staged/. arguments).
func (p *Parser) ValidateCommit(commitish string) bool {
	if IsSpecialArg(commitish) {
		_, err := p.git.Run("status", "--porcelain")
		return err == nil
	}
	_, err := p.git.Run("rev-parse", "--verify", "--quiet", commitish+"^{object}")
	return err == nil
}

// GetBlobContent reads a file at a ref; "working" and "." read the work tree.
func (p *Parser) GetBlobContent(path, ref string) ([]byte, error) {
	data, err := p.getBlobContent(path, ref)
	if err == nil {
		return data, nil
	}
	if errors.Is(err, ErrOutputTooLarge) {
		return nil, fmt.Errorf("Image file %s is too large to display (over 10MB limit)", path)
	}
	if errors.Is(err, ErrInvalidFilePath) || errors.Is(err, ErrPathOutsideRepo) {
		return nil, err
	}
	return nil, fmt.Errorf("Failed to get blob content for %s at %s: %w", path, ref, err)
}

func (p *Parser) getBlobContent(path, ref string) ([]byte, error) {
	if ref == "working" || ref == "." {
		if path == "" {
			return nil, ErrInvalidFilePath
		}
		repo, err := filepath.EvalSymlinks(p.repoPath)
		if err != nil {
			return nil, err
		}
		repo, err = filepath.Abs(repo)
		if err != nil {
			return nil, err
		}
		target := path
		if !filepath.IsAbs(target) {
			target = filepath.Join(repo, filepath.FromSlash(strings.ReplaceAll(path, `\`, "/")))
		}
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(resolved, repo+string(filepath.Separator)) {
			return nil, ErrPathOutsideRepo
		}
		return os.ReadFile(resolved)
	}

	normalized, err := NormalizeRepoRelativePath(p.repoPath, path)
	if err != nil {
		return nil, err
	}
	if ref == "staged" {
		return p.git.RunLimited(maxBlobBytes, "show", ":"+normalized)
	}
	blobHash, err := p.git.RevParse(ref + ":" + normalized)
	if err != nil {
		return nil, err
	}
	return p.git.RunLimited(maxBlobBytes, "cat-file", "blob", blobHash)
}

// GetLineCount counts lines in a file at a ref.
func (p *Parser) GetLineCount(path, ref string) (int, error) {
	data, err := p.GetBlobContent(path, ref)
	if err != nil {
		return 0, err
	}
	count := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		count++
	}
	return count, nil
}

// GetGeneratedStatus checks path rules, .gitattributes, then file headers.
func (p *Parser) GetGeneratedStatus(path, ref string) (isGenerated bool, source string) {
	if generated.Check(path, nil).IsGenerated {
		return true, "path"
	}
	if p.gitattributesGeneratedPaths([]string{path}, ref)[path] {
		return true, "path"
	}
	data, err := p.GetBlobContent(path, ref)
	if err != nil {
		return false, "path"
	}
	head := data[:min(len(data), generatedHeaderScanBytes)]
	lines := strings.Split(toValidUTF8(head), "\n")
	lines = lines[:min(len(lines), generatedHeaderLineLimit)]
	return generated.Check(path, func() []string { return lines }).IsGenerated, "content"
}

// ResolveCommitish returns the short hash for a commit-ish, cached briefly.
func (p *Parser) ResolveCommitish(commitish string) (string, error) {
	now := time.Now()
	p.mu.Lock()
	cached, ok := p.resolvedCommit[commitish]
	p.mu.Unlock()
	if ok && cached.expiresAt.After(now) {
		return cached.value, nil
	}
	hash, err := p.git.RevParse(commitish)
	if err != nil {
		return "", err
	}
	value := ShortHash(hash)
	p.mu.Lock()
	p.resolvedCommit[commitish] = cachedValue{value: value, expiresAt: now.Add(resolvedCommitCacheTTL)}
	p.mu.Unlock()
	return value, nil
}

func (p *Parser) ClearResolvedCommitCache() {
	p.mu.Lock()
	clear(p.resolvedCommit)
	p.mu.Unlock()
}

type localBranch struct {
	name    string
	current bool
}

func (p *Parser) localBranches() ([]localBranch, error) {
	out, err := p.git.RunString("for-each-ref", "--format=%(HEAD)%00%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var branches []localBranch
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		head, name, ok := strings.Cut(line, "\x00")
		if ok && name != "" {
			branches = append(branches, localBranch{name: name, current: head == "*"})
		}
	}
	return branches, nil
}

func (p *Parser) originHeadBranch() (string, bool, error) {
	out, err := p.git.RunString("symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", false, err
	}
	m := originHeadRefPattern.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", false, nil
	}
	return m[1], true, nil
}

// GetDefaultBranch returns origin/HEAD's branch, else a local main/master.
func (p *Parser) GetDefaultBranch() string {
	name, ok, err := p.originHeadBranch()
	if err == nil {
		if ok {
			return name
		}
		return ""
	}
	branches, err := p.localBranches()
	if err != nil {
		return ""
	}
	for _, candidate := range []string{"main", "master"} {
		if slices.ContainsFunc(branches, func(b localBranch) bool { return b.name == candidate }) {
			return candidate
		}
	}
	return ""
}

// GetOriginDefaultBranch returns e.g. "origin/main", or "" if unknown.
func (p *Parser) GetOriginDefaultBranch() string {
	name, ok, err := p.originHeadBranch()
	if err == nil {
		if ok {
			return "origin/" + name
		}
		return ""
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := p.git.Run("show-ref", "--verify", "refs/remotes/origin/"+candidate); err == nil {
			return "origin/" + candidate
		}
	}
	return ""
}

// RevisionOptions lists branches and recent commits for the revision picker.
type RevisionOptions struct {
	Branches            []types.BranchInfo
	Commits             []types.CommitInfo
	OriginDefaultBranch string
	ResolvedBase        string
	ResolvedTarget      string
}

func (p *Parser) GetRevisionOptions(currentBase, currentTarget string) (RevisionOptions, error) {
	branches, err := p.localBranches()
	if err != nil {
		return RevisionOptions{}, err
	}
	logOut, err := p.git.RunString("log", "--max-count=20", "--format=%H%x00%s")
	if err != nil {
		return RevisionOptions{}, err
	}
	defaultBranch := p.GetDefaultBranch()

	result := RevisionOptions{
		Branches:            make([]types.BranchInfo, 0, len(branches)),
		Commits:             []types.CommitInfo{},
		OriginDefaultBranch: p.GetOriginDefaultBranch(),
	}
	for _, b := range branches {
		result.Branches = append(result.Branches, types.BranchInfo{Name: b.name, Current: b.current})
	}
	// Default branch first, then the current branch, then alphabetical.
	slices.SortStableFunc(result.Branches, func(a, b types.BranchInfo) int {
		if defaultBranch != "" {
			if a.Name == defaultBranch {
				return -1
			}
			if b.Name == defaultBranch {
				return 1
			}
		}
		if a.Current != b.Current {
			if a.Current {
				return -1
			}
			return 1
		}
		return collate.Compare(a.Name, b.Name)
	})

	for _, line := range strings.Split(logOut, "\n") {
		hash, subject, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		result.Commits = append(result.Commits, types.CommitInfo{Hash: hash, ShortHash: ShortHash(hash), Message: subject})
	}

	if currentBase != "" && !IsSpecialArg(currentBase) {
		result.ResolvedBase, _ = p.ResolveCommitish(currentBase)
	}
	if currentTarget != "" && !IsSpecialArg(currentTarget) {
		result.ResolvedTarget, _ = p.ResolveCommitish(currentTarget)
	}
	return result, nil
}

// FindUntrackedFiles lists untracked files (not directories) in the repo.
func (p *Parser) FindUntrackedFiles() ([]string, error) {
	out, err := p.git.RunString("status", "--porcelain", "-z", "-uall")
	if err != nil {
		return nil, err
	}
	var files []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		switch {
		case e[:2] == "??":
			files = append(files, e[3:])
		case e[0] == 'R' || e[0] == 'C':
			i++ // renames and copies are followed by the original path
		}
	}
	return files, nil
}

// MarkIntentToAdd runs git add --intent-to-add so untracked files show in git diff.
func (p *Parser) MarkIntentToAdd(files []string) error {
	_, err := p.git.Run(append([]string{"add", "--intent-to-add", "--"}, files...)...)
	return err
}
