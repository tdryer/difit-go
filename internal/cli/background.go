package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"difit-go/internal/proc"
	"difit-go/internal/server"
)

const (
	backgroundChildEnv = "DIFIT_BACKGROUND_CHILD"
	// handshakePrefix marks the line on which a background child reports
	// that it is serving; other stdout lines are ignored.
	handshakePrefix  = "DIFIT_BACKGROUND_READY "
	handshakeTimeout = 10 * time.Second
)

type backgroundInfo struct {
	Port int    `json:"port"`
	URL  string `json:"url"`
	PID  int    `json:"pid"`
}

// backgroundChildArgs drops --background and forces keep-alive and no-open.
func backgroundChildArgs(args []string) []string {
	child := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--background" })
	for _, flag := range []string{"--keep-alive", "--no-open"} {
		if !slices.Contains(child, flag) {
			child = append(child, flag)
		}
	}
	return child
}

// startBackgroundProcess re-runs difit detached and prints the child's
// port, URL and PID as JSON once it is serving.
func (a *app) startBackgroundProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return errors.New("Unable to determine difit entrypoint for background process")
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdoutR.Close()
	defer stderrR.Close()

	cmd := exec.Command(exe, backgroundChildArgs(os.Args[1:])...)
	cmd.Env = append(os.Environ(), backgroundChildEnv+"=1")
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	proc.Detach(cmd)
	err = cmd.Start()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		return err
	}

	handshake := make(chan backgroundInfo, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			line, ok := strings.CutPrefix(scanner.Text(), handshakePrefix)
			if !ok {
				continue
			}
			var info backgroundInfo
			if json.Unmarshal([]byte(line), &info) == nil {
				handshake <- info
				return
			}
		}
	}()
	var stderr bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		io.Copy(&stderr, stderrR)
		close(stderrDone)
	}()
	exited := make(chan int, 1)
	go func() {
		cmd.Wait()
		exited <- cmd.ProcessState.ExitCode()
	}()

	select {
	case info := <-handshake:
		encoded, _ := json.Marshal(info)
		fmt.Fprintln(a.stdout, string(encoded))
		return nil
	case code := <-exited:
		select {
		case <-stderrDone:
		case <-time.After(time.Second):
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			// The caller adds its own "Error: " prefix.
			return errors.New(strings.TrimPrefix(msg, "Error: "))
		}
		return fmt.Errorf("Background difit server exited early (code %d)", code)
	case <-time.After(handshakeTimeout):
		cmd.Process.Kill()
		return errors.New("Timed out while starting background difit server")
	}
}

// runBackgroundChild reports the server to the parent and keeps serving with
// stdio detached.
func (a *app) runBackgroundChild(srv *server.Server) error {
	// The parent closes its ends of our stdout and stderr after the
	// handshake; later writes must not kill the server with SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	encoded, _ := json.Marshal(backgroundInfo{Port: srv.Port, URL: srv.URL, PID: os.Getpid()})
	fmt.Fprintf(a.stdout, "%s%s\n", handshakePrefix, encoded)
	select {}
}
