package mediaid

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const sampleSize int64 = 1024 * 1024

type Result struct {
	Media protocol.Media
	URL   string
	Path  string
}

func Inspect(source, title string, durationSeconds float64) (Result, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Result{}, errors.New("media source is required")
	}
	if remote, ok := canonicalRemoteURL(source); ok {
		if title == "" {
			title = remoteTitle(remote)
		}
		digest := sha256.Sum256([]byte("faro-url-v1\x00" + remote))
		return Result{Media: protocol.Media{
			Title: title, DurationSeconds: durationSeconds,
			Fingerprint: "url-v1:" + hex.EncodeToString(digest[:]),
		}, URL: remote}, nil
	}

	path := source
	if parsed, err := url.Parse(source); err == nil && parsed.Scheme == "file" {
		path, err = url.PathUnescape(parsed.Path)
		if err != nil {
			return Result{}, fmt.Errorf("decode file URL: %w", err)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Result{}, fmt.Errorf("inspect media file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, errors.New("media source is not a regular file")
	}
	fingerprint, err := fileFingerprint(absolute, info.Size())
	if err != nil {
		return Result{}, err
	}
	if title == "" {
		title = filepath.Base(absolute)
	}
	return Result{Media: protocol.Media{
		Title: title, DurationSeconds: durationSeconds, SizeBytes: info.Size(), Fingerprint: fingerprint,
	}, Path: absolute}, nil
}

func canonicalRemoteURL(source string) (string, bool) {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String(), true
}

func remoteTitle(source string) string {
	parsed, _ := url.Parse(source)
	if name := filepath.Base(parsed.Path); name != "." && name != "/" && name != "" {
		return name
	}
	return parsed.Host
}

func fileFingerprint(path string, size int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	_, _ = hash.Write([]byte("faro-file-v1\x00"))
	var encodedSize [8]byte
	binary.BigEndian.PutUint64(encodedSize[:], uint64(size))
	_, _ = hash.Write(encodedSize[:])
	if _, err := io.CopyN(hash, file, min(size, sampleSize)); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if size > sampleSize {
		if _, err := file.Seek(max(sampleSize, size-sampleSize), io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			return "", err
		}
	}
	return "file-v1:" + hex.EncodeToString(hash.Sum(nil)), nil
}
