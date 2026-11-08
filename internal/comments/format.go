package comments

import (
	"fmt"
	"regexp"
	"strings"

	"difit-go/internal/types"
)

var suggestionPattern = regexp.MustCompile("```suggestion\\n((?s:.*?))```")

type suggestionBlock struct {
	code       string
	start, end int
}

func parseSuggestionBlocks(body string) []suggestionBlock {
	var blocks []suggestionBlock
	for _, m := range suggestionPattern.FindAllStringSubmatchIndex(body, -1) {
		blocks = append(blocks, suggestionBlock{
			code:  strings.TrimSuffix(body[m[2]:m[3]], "\n"),
			start: m[0],
			end:   m[1],
		})
	}
	return blocks
}

func lineInfo(line types.LineRange) string {
	if !line.IsRange {
		return fmt.Sprintf("L%d", line.Start)
	}
	return fmt.Sprintf("L%d-L%d", line.Start, line.End)
}

func formatLocation(file string, line types.LineRange, side string) string {
	if file == "" {
		file = "<unknown file>"
	}
	suffix := ""
	if side == "old" {
		suffix = " (old)"
	}
	return file + ":" + lineInfo(line) + suffix
}

func appendSection(result, section string) string {
	if result != "" {
		return result + "\n" + section
	}
	return section
}

func formatContent(body string, codeContent *string) string {
	suggestions := parseSuggestionBlocks(body)
	if len(suggestions) == 0 {
		return body
	}

	result := ""
	last := 0
	for _, s := range suggestions {
		if before := strings.TrimSpace(body[last:s.start]); before != "" {
			result = appendSection(result, before)
		}
		if codeContent != nil && *codeContent != "" {
			result = appendSection(result, "ORIGINAL:\n```\n"+*codeContent+"\n```")
		}
		result = appendSection(result, "SUGGESTED:\n```\n"+s.code+"\n```")
		last = s.end
	}
	if after := strings.TrimSpace(body[last:]); after != "" {
		result = appendSection(result, after)
	}
	return result
}

func formatThread(t types.CommentThread) string {
	sections := []string{formatLocation(t.File, t.Line, t.Side)}
	for i, m := range t.Messages {
		if i > 0 {
			author := strings.TrimSpace(derefString(m.Author))
			if author == "" {
				author = "Unknown"
			}
			sections = append(sections, fmt.Sprintf("Reply %d (%s)", i, author))
		}
		sections = append(sections, formatContent(m.Body, t.CodeContent))
	}
	nonEmpty := sections[:0]
	for _, s := range sections {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n")
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ToCommentThread flattens a thread for text output.
func ToCommentThread(t types.DiffCommentThread) types.CommentThread {
	var code *string
	if t.CodeSnapshot != nil {
		code = &t.CodeSnapshot.Content
	}
	return types.CommentThread{
		ID:          t.ID,
		File:        t.FilePath,
		Line:        t.Position.Line,
		Side:        t.Position.Side,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
		CodeContent: code,
		Messages:    t.Messages,
	}
}

// FormatOutput renders the review summary printed when the server exits.
func FormatOutput(threads []types.DiffCommentThread) string {
	sections := make([]string, len(threads))
	for i, t := range threads {
		sections[i] = formatThread(ToCommentThread(t))
	}
	rule := strings.Repeat("=", 50)
	return strings.Join([]string{
		"\n📝 Comments from review session:",
		rule,
		strings.Join(sections, "\n=====\n"),
		rule,
		fmt.Sprintf("Total comments: %d\n", len(threads)),
	}, "\n")
}
