// Package gitdiff runs git and parses its unified diff output.
package gitdiff

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Git runs git commands in a repository.
type Git struct {
	Dir string
}

// Error carries git's stderr as the message.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	if msg := strings.TrimSpace(e.Stderr); msg != "" {
		return msg
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrOutputTooLarge is returned by RunLimited when output exceeds the limit.
var ErrOutputTooLarge = errors.New("maxBuffer exceeded")

// Run executes git and returns stdout.
func (g Git) Run(args ...string) ([]byte, error) {
	return g.run(0, args...)
}

// RunString executes git and returns stdout as a string.
func (g Git) RunString(args ...string) (string, error) {
	out, err := g.Run(args...)
	return string(out), err
}

// RunLimited executes git, failing with ErrOutputTooLarge if stdout exceeds limit bytes.
func (g Git) RunLimited(limit int, args ...string) ([]byte, error) {
	return g.run(limit, args...)
}

func (g Git) run(limit int, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var out interface{ Write([]byte) (int, error) } = &stdout
	if limit > 0 {
		out = &limitedWriter{buf: &stdout, limit: limit}
	}
	cmd.Stdout = out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if lw, ok := out.(*limitedWriter); ok && lw.exceeded {
		return nil, ErrOutputTooLarge
	}
	if err != nil {
		return nil, &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

type limitedWriter struct {
	buf      *bytes.Buffer
	limit    int
	exceeded bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		w.exceeded = true
		return 0, ErrOutputTooLarge
	}
	return w.buf.Write(p)
}

// RevParse returns the trimmed output of git rev-parse.
func (g Git) RevParse(args ...string) (string, error) {
	out, err := g.RunString(append([]string{"rev-parse"}, args...)...)
	return strings.TrimSpace(out), err
}
