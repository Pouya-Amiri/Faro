//go:build !linux && !darwin && !freebsd && !windows

package mediastream

import "errors"

// freeDiskBytes is unknown here, so streams use the memory cache.
func freeDiskBytes(string) (uint64, error) {
	return 0, errors.New("free disk space is unknown on this platform")
}
