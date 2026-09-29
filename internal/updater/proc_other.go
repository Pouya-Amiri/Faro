//go:build !unix && !windows

package updater

import (
	"os/exec"
	"time"
)

func startDetached(command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func waitForExit(int, time.Duration) {}
