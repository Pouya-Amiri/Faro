//go:build unix

package updater

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// startDetached starts command in its own session so it outlives Faro.
func startDetached(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// waitForExit waits up to timeout for pid to exit. A child of pid knows its
// parent is gone once it has been reparented; that also covers a parent left
// as a zombie, which has already released everything it held.
func waitForExit(pid int, timeout time.Duration) {
	child := os.Getppid() == pid
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if child && os.Getppid() != pid || !child && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
