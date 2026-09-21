//go:build !windows

package youtube

import (
	"context"
	"os/exec"
)

func newCommandContext(ctx context.Context, executable string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, executable, args...)
}
