// Package browser opens URLs in the user's default browser.
package browser

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/tdryer/difit-go/internal/proc"
)

// commands lists the openers to try for the current platform, in order.
func commands(url string) [][]string {
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"open", url}}
	case "windows":
		return [][]string{{"rundll32", "url.dll,FileProtocolHandler", url}}
	}
	cmds := [][]string{{"xdg-open", url}}
	if isWSL() {
		cmds = append([][]string{{"wslview", url}}, cmds...)
	}
	return cmds
}

func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}

// Open launches the browser without waiting for it.
func Open(url string) error {
	err := errors.New("no browser opener available")
	for _, argv := range commands(url) {
		if _, lookErr := exec.LookPath(argv[0]); lookErr != nil {
			err = lookErr
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		if err = proc.StartDetached(cmd); err == nil {
			return nil
		}
	}
	return err
}
