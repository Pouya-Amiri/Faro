//go:build !windows

package mpv

import (
	"io"
	"net"
	"os"
	"path/filepath"
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
	return func() { _ = process.Kill() }, nil
}
