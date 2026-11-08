// Package editor ports the "open in editor" presets and argument templating
// shared with the web client (web/src/utils/editorOptions.ts).
package editor

import (
	"strconv"
	"strings"
	"unicode"
)

const (
	CustomID  = "custom"
	NoneID    = "none"
	DefaultID = "vscode"
)

type Option struct {
	ID           string
	Command      string
	ArgsTemplate string
}

var Options = []Option{
	{"vscode", "code", "-g %file:%line"},
	{"cursor", "cursor", "-g %file:%line"},
	{"zed", "zed", "%file:%line"},
	{"textmate", "mate", "-l %line %file"},
	{"bbedit", "bbedit", "+%line %file"},
	{"emacs", "emacsclient", "--no-wait +%line %file"},
	{"macvim", "mvim", "--remote-silent +%line %file"},
	{"sublime", "subl", "%file:%line"},
	{"nova", "nova", "open %file -l %line"},
	{CustomID, "", ""},
	{NoneID, "", ""},
}

// Resolve looks up a preset by case-insensitive ID, falling back to VS Code.
func Resolve(id string) Option {
	if id == "" {
		id = DefaultID
	}
	id = strings.ToLower(id)
	for _, o := range Options {
		if o.ID == id {
			return o
		}
	}
	return Options[0]
}

// ParseArgsTemplate splits a template into arguments. Single quotes are
// literal; inside double quotes \" and \\ are unescaped and other escapes are
// kept as-is. No shell is involved.
func ParseArgsTemplate(template string) []string {
	input := []rune(template)
	n := len(input)
	tokens := []string{}
	i := 0
	for i < n {
		for i < n && unicode.IsSpace(input[i]) {
			i++
		}
		if i >= n {
			break
		}

		var token strings.Builder
		var quote rune
		for i < n {
			ch := input[i]
			switch {
			case quote == '"':
				if ch == '\\' && i+1 < n && (input[i+1] == '"' || input[i+1] == '\\') {
					token.WriteRune(input[i+1])
					i += 2
					continue
				}
				if ch == '"' {
					quote = 0
				} else {
					token.WriteRune(ch)
				}
				i++
				continue
			case quote == '\'':
				if ch == '\'' {
					quote = 0
				} else {
					token.WriteRune(ch)
				}
				i++
				continue
			case ch == '"' || ch == '\'':
				quote = ch
				i++
				continue
			case unicode.IsSpace(ch):
			default:
				token.WriteRune(ch)
				i++
				continue
			}
			break
		}
		tokens = append(tokens, token.String())
	}
	return tokens
}

// BuildArgs substitutes %file and %line (defaulting to 1) in each argument.
func BuildArgs(argsTemplate, filePath string, line int) []string {
	if line <= 0 {
		line = 1
	}
	lineValue := strconv.Itoa(line)
	args := ParseArgsTemplate(argsTemplate)
	for i, a := range args {
		a = strings.ReplaceAll(a, "%file", filePath)
		args[i] = strings.ReplaceAll(a, "%line", lineValue)
	}
	return args
}
