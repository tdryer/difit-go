package cli

import "os"

type stdinSource string

const (
	stdinPipe   stdinSource = "pipe"
	stdinFile   stdinSource = "file"
	stdinSocket stdinSource = "socket"
	stdinTTY    stdinSource = "tty"
)

func detectStdinSource() stdinSource {
	info, err := os.Stdin.Stat()
	if err != nil {
		return stdinTTY
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeNamedPipe != 0:
		return stdinPipe
	case mode.IsRegular():
		return stdinFile
	case mode&os.ModeSocket != 0:
		return stdinSocket
	}
	return stdinTTY
}

// shouldReadStdin reports whether the diff should come from stdin: when the
// commit-ish is "-", or when no positional arguments were given and stdin is
// not a terminal.
func shouldReadStdin(commitish string, hasPositionalArgs bool, source func() stdinSource) bool {
	if commitish == "-" {
		return true
	}
	if hasPositionalArgs {
		return false
	}
	s := source()
	return s == stdinPipe || s == stdinFile || s == stdinSocket
}
