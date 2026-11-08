package gitdiff

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tdryer/difit-go/internal/generated"
	"github.com/tdryer/difit-go/internal/types"
)

type diffFormat int

const (
	formatGit diffFormat = iota
	formatPlain
)

// ParseUnifiedDiff parses `git diff` output, or plain `diff -u` output when
// no "diff --git" headers are present.
func ParseUnifiedDiff(text string) []types.DiffFile {
	files := []types.DiffFile{}
	if blocks := splitGitDiff(text); len(blocks) > 0 {
		for _, block := range blocks {
			if f, ok := parseFileBlock(block, formatGit); ok {
				files = append(files, f)
			}
		}
		return files
	}
	for _, block := range splitPlainUnifiedDiff(text) {
		if f, ok := parseFileBlock(block, formatPlain); ok {
			files = append(files, f)
		}
	}
	return files
}

const gitHeader = "diff --git "

// splitGitDiff splits text at lines starting with "diff --git ", dropping
// anything before the first one.
func splitGitDiff(text string) []string {
	var starts []int
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], gitHeader) {
			starts = append(starts, i)
		}
		nl := strings.IndexByte(text[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	blocks := make([]string, len(starts))
	for k, start := range starts {
		end := len(text)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		blocks[k] = text[start:end]
	}
	return blocks
}

var hunkCountPattern = regexp.MustCompile(`@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func splitLinesCRLF(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func countOrOne(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

// splitPlainUnifiedDiff splits concatenated `diff -u` outputs into per-file
// blocks, using hunk line counts so content that looks like a header isn't
// mistaken for a new file.
func splitPlainUnifiedDiff(text string) []string {
	lines := splitLinesCRLF(text)
	var blocks []string
	blockStart := -1
	remainingOld, remainingNew := 0, 0

	for i, line := range lines {
		if blockStart >= 0 && strings.HasPrefix(line, "@@") {
			if m := hunkCountPattern.FindStringSubmatch(line); m != nil {
				remainingOld = countOrOne(m[2])
				remainingNew = countOrOne(m[4])
			}
			continue
		}
		if blockStart >= 0 && (remainingOld > 0 || remainingNew > 0) {
			switch {
			case strings.HasPrefix(line, "\\"):
			case strings.HasPrefix(line, "+"):
				remainingNew--
			case strings.HasPrefix(line, "-"):
				remainingOld--
			case strings.HasPrefix(line, " "):
				remainingOld--
				remainingNew--
			}
			continue
		}
		if strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
			if blockStart >= 0 {
				blocks = append(blocks, strings.Join(lines[blockStart:i], "\n"))
			}
			blockStart = i
		}
	}
	if blockStart >= 0 {
		blocks = append(blocks, strings.Join(lines[blockStart:], "\n"))
	}
	return blocks
}

var gitPathPrefixes = []string{"a/", "b/", "c/", "i/", "w/"}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// decodeGitPath strips quotes, timestamps, a/ b/-style prefixes and C-style
// escapes from a path as printed by git. It returns ok=false for /dev/null.
func decodeGitPath(raw string, stripPrefix bool) (string, bool) {
	if i := strings.IndexByte(raw, '\t'); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	} else if raw == `"` {
		raw = ""
	}
	if stripPrefix {
		for _, p := range gitPathPrefixes {
			if strings.HasPrefix(raw, p) {
				raw = raw[len(p):]
				break
			}
		}
	}
	if raw == "/dev/null" {
		return "", false
	}

	var out []byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '\\' || i+1 >= len(raw) {
			out = append(out, c)
			continue
		}
		next := raw[i+1]
		if isOctal(next) {
			value := int(next - '0')
			read := 1
			for read < 3 && i+1+read < len(raw) && isOctal(raw[i+1+read]) {
				value = value*8 + int(raw[i+1+read]-'0')
				read++
			}
			out = append(out, byte(value))
			i += read
			continue
		}
		switch next {
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'v':
			out = append(out, '\v')
		case 'a':
			out = append(out, '\a')
		default:
			out = append(out, next)
		}
		i++
	}
	return toValidUTF8(out), true
}

// toValidUTF8 replaces each invalid byte with U+FFFD, like Node's Buffer.toString.
func toValidUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		sb.WriteRune(r)
		b = b[size:]
	}
	return sb.String()
}

func extractPathFromLine(line, prefix string, stripPrefix bool) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	return decodeGitPath(line[len(prefix):], stripPrefix)
}

// parseDiffHeaderPaths splits "diff --git <old> <new>", honoring quotes and
// backslash-escaped spaces. ok is false when the split is ambiguous.
func parseDiffHeaderPaths(header string) (oldPath string, hasOld bool, newPath string, hasNew bool, ok bool) {
	if !strings.HasPrefix(header, gitHeader) {
		return "", false, "", false, false
	}
	raw := header[len(gitHeader):]
	var segments []string
	var current strings.Builder
	inQuotes := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		escaped := i > 0 && raw[i-1] == '\\'
		if c == '"' && !escaped {
			inQuotes = !inQuotes
			current.WriteByte(c)
			continue
		}
		if c == ' ' && !inQuotes && !escaped {
			if current.Len() > 0 {
				segments = append(segments, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteByte(c)
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	if len(segments) != 2 {
		return "", false, "", false, false
	}
	oldPath, hasOld = decodeGitPath(segments[0], true)
	newPath, hasNew = decodeGitPath(segments[1], true)
	return oldPath, hasOld, newPath, hasNew, true
}

func findLine(lines []string, prefix string) (string, bool) {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l, true
		}
	}
	return "", false
}

type optPath struct {
	value string
	ok    bool
}

func firstPath(paths ...optPath) optPath {
	for _, p := range paths {
		if p.ok {
			return p
		}
	}
	return optPath{}
}

func parseFileBlock(block string, format diffFormat) (types.DiffFile, bool) {
	lines := strings.Split(block, "\n")
	var header optPath
	var oldHeader optPath
	if o, hasO, n, hasN, ok := parseDiffHeaderPaths(lines[0]); ok {
		oldHeader = optPath{o, hasO}
		header = optPath{n, hasN}
	}

	minusLine, hasMinus := findLine(lines, "--- ")
	plusLine, hasPlus := findLine(lines, "+++ ")
	renameFromLine, _ := findLine(lines, "rename from ")
	renameToLine, _ := findLine(lines, "rename to ")

	stripPrefix := format != formatPlain
	pathOf := func(line, prefix string, strip bool) optPath {
		v, ok := extractPathFromLine(line, prefix, strip)
		return optPath{v, ok}
	}
	plusPath := pathOf(plusLine, "+++ ", stripPrefix)
	minusPath := pathOf(minusLine, "--- ", stripPrefix)
	renameFrom := pathOf(renameFromLine, "rename from ", true)
	renameTo := pathOf(renameToLine, "rename to ", true)

	parsedNew := firstPath(renameTo, plusPath, header)
	parsedOld := firstPath(renameFrom, minusPath, oldHeader)
	newPath := firstPath(parsedNew, parsedOld)
	oldPath := firstPath(parsedOld, newPath)
	if !newPath.ok || newPath.value == "" {
		return types.DiffFile{}, false
	}
	path := newPath.value

	_, newFileMode := findLine(lines, "new file mode")
	_, deletedFileMode := findLine(lines, "deleted file mode")

	status := "modified"
	switch {
	case newFileMode || (hasMinus && strings.Contains(minusLine, "/dev/null")):
		status = "added"
	case deletedFileMode || (hasPlus && strings.Contains(plusLine, "/dev/null")):
		status = "deleted"
	case format != formatPlain && oldPath.value != newPath.value:
		status = "renamed"
	}

	file := types.DiffFile{
		Path:        path,
		Status:      status,
		Chunks:      parseChunks(lines),
		IsGenerated: generated.Check(path, nil).IsGenerated,
	}
	if status == "renamed" {
		file.OldPath = oldPath.value
	}
	for _, c := range file.Chunks {
		for _, l := range c.Lines {
			switch l.Type {
			case "add":
				file.Additions++
			case "delete":
				file.Deletions++
			}
		}
	}
	return file, true
}

var hunkHeaderPattern = regexp.MustCompile(`@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)`)

func parseChunks(lines []string) []types.DiffChunk {
	chunks := []types.DiffChunk{}
	var current *types.DiffChunk
	oldNum, newNum := 0, 0

	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if current != nil {
				chunks = append(chunks, *current)
				current = nil
			}
			m := hunkHeaderPattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			oldStart, _ := strconv.Atoi(m[1])
			newStart, _ := strconv.Atoi(m[3])
			oldNum, newNum = oldStart, newStart
			current = &types.DiffChunk{
				Header:   line,
				OldStart: oldStart,
				OldLines: countOrOne(m[2]),
				NewStart: newStart,
				NewLines: countOrOne(m[4]),
				Lines:    []types.DiffLine{},
			}
			continue
		}
		if current == nil || line == "" {
			continue
		}
		var typ string
		switch line[0] {
		case '+':
			typ = "add"
		case '-':
			typ = "delete"
		case ' ':
			typ = "normal"
		default:
			continue
		}
		dl := types.DiffLine{Type: typ, Content: toValidUTF8([]byte(line[1:]))}
		if typ != "add" {
			dl.OldLineNumber = types.Ptr(oldNum)
			oldNum++
		}
		if typ != "delete" {
			dl.NewLineNumber = types.Ptr(newNum)
			newNum++
		}
		current.Lines = append(current.Lines, dl)
	}
	if current != nil {
		chunks = append(chunks, *current)
	}
	return chunks
}
