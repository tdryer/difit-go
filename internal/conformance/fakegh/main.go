// Command fakegh stands in for the GitHub CLI in the conformance tests. It
// answers `gh pr diff` and `gh api graphql` with canned files from
// $FAKE_GH_DIR and appends each invocation's arguments to calls.jsonl there.
//
// A response file <name> is printed to stdout; <name>.err instead makes the
// command fail with that file's content on stderr.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dir := os.Getenv("FAKE_GH_DIR")
	args := os.Args[1:]
	if line, err := json.Marshal(args); err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}

	switch {
	case len(args) >= 2 && args[0] == "pr" && args[1] == "diff":
		respond(dir, "pr.patch")
	case len(args) >= 2 && args[0] == "api" && args[1] == "graphql":
		page := "1"
		for _, arg := range args {
			if cursor, ok := strings.CutPrefix(arg, "endCursor="); ok {
				page = cursor
			}
		}
		respond(dir, "graphql-"+page+".json")
	default:
		fmt.Fprintf(os.Stderr, "fakegh: unsupported command %q\n", args)
		os.Exit(2)
	}
}

func respond(dir, name string) {
	if msg, err := os.ReadFile(filepath.Join(dir, name+".err")); err == nil {
		os.Stderr.Write(msg)
		os.Exit(1)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakegh:", err)
		os.Exit(1)
	}
	os.Stdout.Write(data)
}
