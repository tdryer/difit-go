// Package proc starts processes that outlive difit.
package proc

import "os/exec"

// StartDetached starts cmd in its own session (or console on Windows) so it
// survives difit exiting and is not hit by terminal signals.
func StartDetached(cmd *exec.Cmd) error {
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child in the background; its exit status is irrelevant.
	go cmd.Wait()
	return nil
}

// Detach configures cmd to start in its own session without starting it.
func Detach(cmd *exec.Cmd) { detach(cmd) }
