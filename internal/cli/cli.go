// Package cli implements the difit command line.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tdryer/difit-go/internal/browser"
	"github.com/tdryer/difit-go/internal/comments"
	"github.com/tdryer/difit-go/internal/gitdiff"
	"github.com/tdryer/difit-go/internal/github"
	"github.com/tdryer/difit-go/internal/server"
	"github.com/tdryer/difit-go/internal/types"
	"github.com/tdryer/difit-go/internal/web"
)

// exitError carries an exit status for a failure that was already reported.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	version        string
	stdinSource    func() stdinSource
}

// fail prints "Error: message" and returns an exit status of 1.
func (a *app) fail(format string, args ...any) error {
	fmt.Fprintf(a.stderr, "Error: "+format+"\n", args...)
	return exitError{1}
}

type options struct {
	port             int
	host             string
	noOpen           bool
	comment          []string
	pr               string
	clean            bool
	includeUntracked bool
	keepAlive        bool
	background       bool
	context          string
	mergeBase        bool
}

// Main runs difit with args (excluding the program name) and returns the
// process exit status.
func Main(args []string, version string) int {
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, version: version, stdinSource: detectStdinSource}
	return a.run(args)
}

func (a *app) run(args []string) int {
	root := a.rootCommand()
	root.SetArgs(args)
	err := root.Execute()
	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	default:
		fmt.Fprintf(a.stderr, "error: %v\n", err)
		return 1
	}
}

func (a *app) rootCommand() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:     "difit [commit-ish] [compare-with]",
		Short:   "A lightweight Git diff viewer with GitHub-like interface",
		Version: a.version,
		Args:    cobra.MaximumNArgs(2),
		Long: `A lightweight Git diff viewer with GitHub-like interface

Arguments:
  commit-ish    Git commit, tag, branch, HEAD~n reference, or "working"/"staged"/"." (default: "HEAD")
  compare-with  Optional: Compare with this commit/branch (shows diff between commit-ish and compare-with)`,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runMain(cmd, args, opts)
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetIn(a.stdin)
	cmd.SetOut(a.stdout)
	cmd.SetErr(a.stderr)
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.Flags().BoolP("version", "v", false, "output the version number")

	f := cmd.Flags()
	f.SortFlags = false
	f.IntVar(&opts.port, "port", 0, "preferred `port` (auto-assigned if occupied)")
	f.StringVar(&opts.host, "host", "", "`host` address to bind")
	f.BoolVar(&opts.noOpen, "no-open", false, "do not automatically open browser")
	f.StringArrayVar(&opts.comment, "comment", nil, "inject initial review comments (repeatable, accepts a `json` object or array)")
	f.StringVar(&opts.pr, "pr", "", "GitHub PR `url` to review (e.g., https://github.com/owner/repo/pull/123)")
	f.BoolVar(&opts.clean, "clean", false, "start with a clean slate by clearing all existing comments")
	f.BoolVar(&opts.includeUntracked, "include-untracked", false, "automatically include untracked files in diff")
	f.BoolVar(&opts.keepAlive, "keep-alive", false, "keep server running even after browser disconnects")
	f.BoolVar(&opts.background, "background", false, "keep the server running in the background and output JSON info")
	f.StringVar(&opts.context, "context", "", "number of context `lines` shown around each change")
	f.BoolVar(&opts.mergeBase, "merge-base", false, "resolve the base revision with git merge-base before diffing (Git revision mode only)")

	cmd.AddCommand(a.commentCommand())
	return cmd
}

// parseIntJS mimics parseInt(s, 10): leading whitespace and trailing junk
// are ignored, and ok is false when no digits were found.
func parseIntJS(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r\v\f")
	end := 0
	if end < len(s) && (s[end] == '+' || s[end] == '-') {
		end++
	}
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == digits {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

// resolveDiffSelection derives the base from the target when no explicit
// comparison is given.
func resolveDiffSelection(commitish, compareWith string, mergeBase bool) types.DiffSelection {
	var base string
	switch {
	case compareWith != "":
		base = compareWith
	case commitish == "working":
		base = "staged"
	case gitdiff.IsSpecialArg(commitish):
		base = "HEAD"
	default:
		base = commitish + "^"
	}
	mode := ""
	if mergeBase {
		mode = types.BaseModeMergeBase
	}
	return types.NewDiffSelection(base, commitish, mode)
}

func determineDiffMode(sel types.DiffSelection, compareWith string) types.DiffMode {
	target := sel.TargetCommitish
	// Comparing specific commits needs no watching, except for "." which
	// still reflects the working tree.
	if compareWith != "" && target != "HEAD" && target != "." {
		return types.DiffModeSpecific
	}
	switch target {
	case "working":
		return types.DiffModeWorking
	case "staged":
		return types.DiffModeStaged
	case ".":
		return types.DiffModeDot
	}
	return types.DiffModeDefault
}

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", errors.New("Not a git repository (or any of the parent directories)")
	}
	return strings.TrimSpace(string(out)), nil
}

func (a *app) runMain(cmd *cobra.Command, args []string, opts options) error {
	commitish, compareWith := "HEAD", ""
	if len(args) > 0 {
		commitish = args[0]
	}
	if len(args) > 1 {
		compareWith = args[1]
	}

	backgroundChild := os.Getenv(backgroundChildEnv) == "1"
	backgroundMode := opts.background || backgroundChild
	contextLines := -1
	contextSet := cmd.Flags().Changed("context")
	if contextSet {
		n, ok := parseIntJS(opts.context)
		if !ok || n < 0 {
			return a.fail("--context must be a non-negative integer")
		}
		contextLines = n
	}

	if opts.background && !backgroundChild {
		if err := a.startBackgroundProcess(); err != nil {
			return a.fail("%s", err.Error())
		}
		return nil
	}

	var manualImports []types.CommentImport
	for _, value := range opts.comment {
		imports, err := comments.ParseImportValue(value)
		if err != nil {
			return a.fail("%s", err.Error())
		}
		manualImports = append(manualImports, imports...)
	}
	commentImports := manualImports

	if backgroundMode {
		opts.keepAlive = true
		opts.noOpen = true
	}

	var stdinDiff string
	reviewLabel := "diff from stdin"
	if opts.pr != "" {
		if commitish != "HEAD" || compareWith != "" {
			return a.fail("--pr option cannot be used with positional arguments")
		}
		if opts.mergeBase {
			return a.fail("--merge-base option cannot be used with --pr")
		}
		if contextSet {
			return a.fail("--context option cannot be used with --pr")
		}
		patch, err := github.GetPRPatch(opts.pr)
		if err != nil {
			fmt.Fprintf(a.stderr, "Error resolving PR: %s\n", err)
			return exitError{1}
		}
		stdinDiff, reviewLabel = patch, opts.pr

		warn := func(s string) { fmt.Fprintln(a.stderr, s) }
		prImports, err := github.GetPRCommentImports(opts.pr, warn)
		if err != nil {
			fmt.Fprintf(a.stderr, "Warning: Failed to load PR review comments: %s\n", err)
		} else {
			commentImports = append(prImports, manualImports...)
		}
	} else if shouldReadStdin(commitish, len(args) > 0, a.stdinSource) {
		if contextSet {
			return a.fail("--context option cannot be used with stdin diff")
		}
		if opts.mergeBase {
			return a.fail("--merge-base option cannot be used with stdin diff")
		}
		data, err := io.ReadAll(a.stdin)
		if err != nil {
			return a.fail("%s", err.Error())
		}
		stdinDiff = string(data)
		if strings.TrimSpace(stdinDiff) == "" {
			return a.fail("No diff content received from stdin")
		}
	}

	serverOpts := server.Options{
		StdinDiff:      stdinDiff,
		PreferredPort:  opts.port,
		Host:           opts.host,
		OpenBrowser:    !opts.noOpen,
		ClearComments:  opts.clean,
		KeepAlive:      opts.keepAlive,
		CommentImports: commentImports,
		ContextLines:   -1,
		Static:         web.FS(),
		Stdout:         a.stdout,
		Stderr:         a.stderr,
		OpenURL:        browser.Open,
	}

	if stdinDiff != "" {
		srv, err := server.Start(serverOpts)
		if err != nil {
			return a.fail("%s", err.Error())
		}
		if backgroundMode {
			return a.runBackgroundChild(srv)
		}
		fmt.Fprintf(a.stdout, "\n🚀 difit server started on %s\n", srv.URL)
		fmt.Fprintf(a.stdout, "📋 Reviewing: %s\n", reviewLabel)
		if opts.keepAlive {
			fmt.Fprintln(a.stdout, "🔒 Keep-alive mode: server will stay running after browser disconnects")
		}
		fmt.Fprintln(a.stdout, "\nPress Ctrl+C to stop the server")
		return a.serveUntilDone(srv)
	}

	repoPath, err := gitRoot()
	if err != nil {
		// Outside a repository, fall back to the working directory.
		repoPath = ""
	}

	sel := resolveDiffSelection(commitish, compareWith, opts.mergeBase)
	if opts.mergeBase && gitdiff.IsSpecialArg(sel.BaseCommitish) {
		return a.fail("--merge-base requires a commit-ish base, but resolved base was %q", sel.BaseCommitish)
	}

	if sel.TargetCommitish == "working" || sel.TargetCommitish == "." {
		// Skip interactive prompts in detached background mode.
		if !backgroundChild || opts.includeUntracked {
			if err := a.handleUntrackedFiles(gitdiff.NewParser(repoPath), opts.includeUntracked); err != nil {
				return a.fail("%s", err.Error())
			}
		}
	}

	if err := gitdiff.ValidateDiffArguments(sel.TargetCommitish, compareWith, len(args) > 1); err != nil {
		return a.fail("%s", err.Error())
	}

	serverOpts.Selection = sel
	serverOpts.ContextLines = contextLines
	serverOpts.DiffMode = determineDiffMode(sel, compareWith)
	serverOpts.RepoPath = repoPath
	srv, err := server.Start(serverOpts)
	if err != nil {
		return a.fail("%s", err.Error())
	}
	if backgroundMode {
		return a.runBackgroundChild(srv)
	}

	fmt.Fprintf(a.stdout, "\n🚀 difit server started on %s\n", srv.URL)
	fmt.Fprintf(a.stdout, "📋 Reviewing: %s\n", sel.TargetCommitish)
	if opts.keepAlive {
		fmt.Fprintln(a.stdout, "🔒 Keep-alive mode: server will stay running after browser disconnects")
	}
	if opts.clean {
		fmt.Fprintln(a.stdout, "🧹 Starting with a clean slate - all existing comments will be cleared")
	}
	switch {
	case srv.IsEmpty():
		fmt.Fprintln(a.stdout, "\n! \x1b[33mNo differences found. Browser will not open automatically.\x1b[0m")
		fmt.Fprintf(a.stdout, "   Server is running at %s if you want to check manually.\n\n", srv.URL)
	case !opts.noOpen:
		fmt.Fprintln(a.stdout, "🌐 Opening browser...")
		fmt.Fprintln(a.stdout)
	default:
		fmt.Fprintln(a.stdout, "💡 Use --open to automatically open browser")
		fmt.Fprintln(a.stdout)
	}
	return a.serveUntilDone(srv)
}

// serveUntilDone blocks until Ctrl+C or the browser tab closes, then prints
// the review comments.
func (a *app) serveUntilDone(srv *server.Server) error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	select {
	case <-interrupt:
		fmt.Fprintln(a.stdout, "\n👋 Shutting down difit server...")
	case <-srv.ShutdownRequested():
	}
	output := srv.FinalComments()
	srv.Close()
	if strings.TrimSpace(output) != "" {
		fmt.Fprintln(a.stdout, output)
	}
	return nil
}

func (a *app) handleUntrackedFiles(parser *gitdiff.Parser, addAutomatically bool) error {
	files, err := parser.FindUntrackedFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	if addAutomatically || a.promptIncludeUntracked(files) {
		if err := parser.MarkIntentToAdd(files); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "✅ Files added with --intent-to-add")
		fmt.Fprintf(a.stdout, "   💡 To undo this, run `git reset -- %s`\n", strings.Join(files, " "))
	} else {
		fmt.Fprintln(a.stdout, "i Untracked files will not be shown in diff")
	}
	return nil
}

func (a *app) promptIncludeUntracked(files []string) bool {
	fmt.Fprintf(a.stdout, "\n📝 Found %d untracked file(s):\n", len(files))
	for _, f := range files {
		fmt.Fprintf(a.stdout, "    - %s\n", f)
	}
	fmt.Fprint(a.stdout, "\n❓ Would you like to include these untracked files in the diff review? (Y/n): ")
	answer, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && answer == "" {
		// No answer at all (stdin closed) counts as no.
		fmt.Fprintln(a.stdout)
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes"
}
