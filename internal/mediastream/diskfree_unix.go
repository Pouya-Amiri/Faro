//go:build linux || darwin || freebsd

package mediastream

import "golang.org/x/sys/unix"

func freeDiskBytes(dir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
