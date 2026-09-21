//go:build windows

package youtube

import (
	"context"
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func newCommandContext(ctx context.Context, executable string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return command
}
