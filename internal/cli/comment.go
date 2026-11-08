package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tdryer/difit-go/internal/comments"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func serverURL(port int, path string) string {
	return "http://localhost:" + strconv.Itoa(port) + path
}

// commandError reports a failed request, distinguishing an unreachable
// server from other failures.
func (a *app) commandError(err error, port int) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return a.fail("Cannot connect to difit server on port %d. Is the server running?", port)
	}
	return a.fail("%s", err.Error())
}

func errorMessage(body []byte, fallback string) string {
	var parsed struct {
		Error *string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil {
		return *parsed.Error
	}
	return fallback
}

func portFlag(cmd *cobra.Command, port *int) {
	cmd.Flags().IntVar(port, "port", 0, "port of the running difit server")
	cmd.MarkFlagRequired("port")
}

func (a *app) commentCommand() *cobra.Command {
	comment := &cobra.Command{
		Use:   "comment",
		Short: "Add, retrieve, or resolve comments on a running difit server",
		Args:  cobra.NoArgs,
	}

	var addPort int
	add := &cobra.Command{
		Use:   "add [json]",
		Short: "Add comments to a running difit server",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.commentAdd(args, addPort)
		},
	}
	portFlag(add, &addPort)

	var getPort int
	var format string
	get := &cobra.Command{
		Use:   "get",
		Short: "Retrieve comments from a running difit server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				fmt.Fprintf(a.stderr, "error: option '--format <format>' argument '%s' is invalid. Allowed choices are text, json.\n", format)
				return exitError{1}
			}
			return a.commentGet(getPort, format)
		},
	}
	portFlag(get, &getPort)
	get.Flags().StringVar(&format, "format", "text", `output format (choices: "text", "json")`)

	var resolvePort int
	resolve := &cobra.Command{
		Use:     "resolve <threadIds...>",
		Aliases: []string{"remove"},
		Short:   "Resolve (remove) comment threads on a running difit server",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.commentResolve(args, resolvePort)
		},
	}
	portFlag(resolve, &resolvePort)

	comment.AddCommand(add, get, resolve)
	return comment
}

func (a *app) commentAddInput(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if a.stdinSource() == stdinTTY {
		return "", errors.New("Provide comment JSON as an argument or via stdin")
	}
	data, err := io.ReadAll(a.stdin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", errors.New("No comment JSON received from stdin")
	}
	return string(data), nil
}

func (a *app) commentAdd(args []string, port int) error {
	input, err := a.commentAddInput(args)
	if err != nil {
		return a.fail("%s", err.Error())
	}
	imports, err := comments.ParseImportValue(input)
	if err != nil {
		return a.fail("%s", err.Error())
	}
	body, err := comments.MarshalJSON(imports)
	if err != nil {
		return a.fail("%s", err.Error())
	}
	resp, err := httpClient.Post(serverURL(port, "/api/comment-imports"), "application/json", bytes.NewReader(body))
	if err != nil {
		return a.commandError(err, port)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return a.commandError(err, port)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return a.fail("%s", errorMessage(data, "Failed to add comments"))
	}

	var result struct {
		Success  *bool     `json:"success"`
		ImportID *string   `json:"importId"`
		Count    *int      `json:"count"`
		Warnings *[]string `json:"warnings"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return a.fail("%s", err.Error())
	}
	out := struct {
		Success  bool     `json:"success"`
		ImportID *string  `json:"importId,omitempty"`
		Count    int      `json:"count"`
		Warnings []string `json:"warnings"`
	}{true, result.ImportID, len(imports), []string{}}
	if result.Success != nil {
		out.Success = *result.Success
	}
	if result.Count != nil {
		out.Count = *result.Count
	}
	if result.Warnings != nil {
		out.Warnings = *result.Warnings
	}
	encoded, _ := comments.MarshalJSON(out)
	a.stdout.Write(encoded)
	fmt.Fprintln(a.stdout)
	return nil
}

func (a *app) commentGet(port int, format string) error {
	endpoint := "/api/comments-output"
	if format == "json" {
		endpoint = "/api/comments-json"
	}
	resp, err := httpClient.Get(serverURL(port, endpoint))
	if err != nil {
		return a.commandError(err, port)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return a.commandError(err, port)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return a.fail("Failed to retrieve comments")
	}
	if format == "json" {
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return a.fail("%s", err.Error())
		}
		fmt.Fprintln(a.stdout, compact.String())
	} else if strings.TrimSpace(string(data)) != "" {
		fmt.Fprintln(a.stdout, string(data))
	}
	return nil
}

type resolveFailure struct {
	ThreadID string `json:"threadId"`
	Error    string `json:"error"`
}

func (a *app) commentResolve(threadIDs []string, port int) error {
	resolved, notFound, failures := []string{}, []string{}, []resolveFailure{}
	for _, id := range threadIDs {
		req, err := http.NewRequest(http.MethodDelete, serverURL(port, "/api/comments/"+url.PathEscape(id)), nil)
		if err != nil {
			return a.fail("%s", err.Error())
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return a.commandError(err, port)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode <= 299:
			resolved = append(resolved, id)
		case resp.StatusCode == http.StatusNotFound:
			notFound = append(notFound, id)
		default:
			failures = append(failures, resolveFailure{id, errorMessage(data, "Failed to resolve thread "+id)})
		}
	}

	ok := len(notFound) == 0 && len(failures) == 0
	encoded, _ := comments.MarshalJSON(struct {
		Success  bool             `json:"success"`
		Resolved []string         `json:"resolved"`
		NotFound []string         `json:"notFound"`
		Errors   []resolveFailure `json:"errors"`
	}{ok, resolved, notFound, failures})
	a.stdout.Write(encoded)
	fmt.Fprintln(a.stdout)
	if !ok {
		return exitError{1}
	}
	return nil
}
