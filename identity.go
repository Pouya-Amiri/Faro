package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// loadIdentity returns this installation's secret identity, creating it on
// first use. Rooms see only values derived from it, one per room, which is
// what lets a ban hold after the banned person reconnects.
func loadIdentity() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return loadIdentityFrom(filepath.Join(root, "faro", "identity"))
}

func loadIdentityFrom(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if identity := strings.TrimSpace(string(data)); identityPattern.MatchString(identity) {
			return identity, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	identity := base64.RawURLEncoding.EncodeToString(secret)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(identity+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, path); err != nil {
		return "", err
	}
	return identity, nil
}
