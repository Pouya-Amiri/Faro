package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// maximumDownload guards against a runaway response; Faro's packages are
	// a few tens of megabytes.
	maximumDownload  = 1 << 30
	downloadAttempts = 4
	// stallTimeout abandons an attempt that has received nothing for this
	// long. The next attempt resumes where it stopped.
	stallTimeout = 45 * time.Second
)

var errChecksumMismatch = errors.New("the downloaded update does not match its published checksum")

type progressFunc func(received, total int64)

// download fetches item into dir and returns the path of the complete file,
// whose SHA-256 has been checked against want. A partial file left by an
// interrupted download is resumed, and a complete, verified file is reused.
func download(ctx context.Context, client *http.Client, item asset, want, dir, userAgent string, progress progressFunc) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, item.name)
	if fileHash(final) == want {
		// Hashing a cached file takes a moment; a cancellation meanwhile wins.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		progress(item.size, item.size)
		return final, nil
	}
	_ = os.Remove(final)
	part := final + ".part"
	file, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	offset, err := io.Copy(digest, file)
	if err != nil || item.size > 0 && offset > item.size {
		if offset, err = restart(file, digest); err != nil {
			return "", err
		}
	}

	var lastErr error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		var retry bool
		offset, retry, lastErr = fetchRange(ctx, client, item, file, digest, offset, userAgent, progress)
		if lastErr == nil || !retry || ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if lastErr != nil {
		return "", lastErr
	}
	if item.size > 0 && offset != item.size {
		return "", fmt.Errorf("the download ended after %d of %d bytes", offset, item.size)
	}
	if hex.EncodeToString(digest.Sum(nil)) != want {
		_ = file.Close()
		_ = os.Remove(part)
		return "", errChecksumMismatch
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

// fetchRange downloads from offset to the end of item into file. It reports
// the new offset and whether a failure is worth another attempt.
func fetchRange(ctx context.Context, client *http.Client, item asset, file *os.File, digest hash.Hash, offset int64, userAgent string, progress progressFunc) (int64, bool, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, item.url, nil)
	if err != nil {
		return offset, false, err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/octet-stream")
	if offset > 0 {
		request.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	response, err := client.Do(request)
	if err != nil {
		return offset, true, fmt.Errorf("download the update: %w", err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusPartialContent && offset > 0 && rangeStart(response.Header.Get("Content-Range")) == offset:
	case response.StatusCode == http.StatusOK:
		if offset, err = restart(file, digest); err != nil {
			return 0, false, err
		}
	case response.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		// The partial file is already complete, or is not a prefix of this
		// asset; checksum verification or the next attempt settles which.
		if item.size > 0 && offset == item.size {
			return offset, false, nil
		}
		offset, err = restart(file, digest)
		return offset, err == nil, errors.New("the server rejected the resumed download")
	default:
		retry := response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests
		return offset, retry, fmt.Errorf("download the update: server returned %s", response.Status)
	}
	total := item.size
	if total <= 0 && response.ContentLength > 0 {
		total = offset + response.ContentLength
	}
	if total > maximumDownload {
		return offset, false, errors.New("the update is unexpectedly large")
	}

	// A watchdog cancels the request when no data arrives for stallTimeout.
	stalled := time.AfterFunc(stallTimeout, cancel)
	defer stalled.Stop()
	buffer := make([]byte, 64<<10)
	lastReport := time.Time{}
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			stalled.Reset(stallTimeout)
			if offset+int64(count) > maximumDownload || item.size > 0 && offset+int64(count) > item.size {
				return offset, false, errors.New("the update is larger than published")
			}
			if _, err := file.Write(buffer[:count]); err != nil {
				return offset, false, err
			}
			digest.Write(buffer[:count])
			offset += int64(count)
			if now := time.Now(); now.Sub(lastReport) >= 100*time.Millisecond {
				lastReport = now
				progress(offset, total)
			}
		}
		if readErr == io.EOF {
			progress(offset, total)
			if item.size > 0 && offset < item.size {
				// A body that ends cleanly but early, as a proxy may send,
				// is resumed like a dropped connection.
				return offset, true, fmt.Errorf("the download ended after %d of %d bytes", offset, item.size)
			}
			return offset, false, nil
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return offset, false, ctx.Err()
			}
			return offset, true, fmt.Errorf("download the update: %w", readErr)
		}
	}
}

func restart(file *os.File, digest hash.Hash) (int64, error) {
	digest.Reset()
	if err := file.Truncate(0); err != nil {
		return 0, err
	}
	_, err := file.Seek(0, io.SeekStart)
	return 0, err
}

// rangeStart returns the first byte of a "bytes start-end/size" header, or -1.
func rangeStart(header string) int64 {
	value, found := strings.CutPrefix(strings.TrimSpace(header), "bytes ")
	if !found {
		return -1
	}
	start, _, found := strings.Cut(value, "-")
	if !found {
		return -1
	}
	parsed, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return -1
	}
	return parsed
}

// fileHash returns the hex SHA-256 of path, or "" when it cannot be read.
func fileHash(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return ""
	}
	return hex.EncodeToString(digest.Sum(nil))
}
