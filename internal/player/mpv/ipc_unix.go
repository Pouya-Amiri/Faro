//go:build !windows

package mpv

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func ipcAddress(id string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "faro-mpv-*")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(directory, "ipc.sock"), func() { _ = os.RemoveAll(directory) }, nil
}

func dialIPC(path string) (io.ReadWriteCloser, error) { return net.Dial("unix", path) }

func assignProcessCleanup(process *os.Process) (func(), error) {
	// SIGTERM lets wrappers such as iina-cli forward termination to the real
	// application. SIGKILL would terminate only the wrapper and leave IINA open.
	return func() { _ = process.Signal(syscall.SIGTERM) }, nil
}
