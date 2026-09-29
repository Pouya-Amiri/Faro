//go:build !linux && !darwin && !windows

package updater

import (
	"context"
	"errors"
)

func detectInstallation() installation {
	return installation{method: MethodManual, reason: "Automatic updates aren't available on this system."}
}

func applyUpdate(context.Context, installation, string, string) (func() error, error) {
	return nil, errors.New("this installation can't be updated automatically")
}

func openDownload(string) error {
	return errors.New("opening downloads isn't supported on this system")
}

func cleanupInstallation() {}
