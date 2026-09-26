package client

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrFingerprintMismatch means the server's certificate is not the one the
// invite pinned: retrying cannot succeed until the invite is replaced.
var ErrFingerprintMismatch = errors.New("server certificate fingerprint mismatch")

func tlsConfig(serverName, fingerprint string) (*tls.Config, error) {
	if fingerprint == "" {
		return &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName}, nil
	}
	expected, err := normalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Verification is replaced with exact certificate pinning below. This is
		// intentionally not a user-selectable insecure mode.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("server did not provide a certificate")
			}
			actual := sha256.Sum256(state.PeerCertificates[0].Raw)
			if !equalDigest(actual[:], expected) {
				return fmt.Errorf("%w: got %s", ErrFingerprintMismatch, hex.EncodeToString(actual[:]))
			}
			return nil
		},
	}, nil
}

func normalizeFingerprint(value string) ([]byte, error) {
	value = strings.NewReplacer(":", "", "-", "", " ", "").Replace(strings.ToLower(value))
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("certificate fingerprint must be a SHA-256 digest")
	}
	return decoded, nil
}

func equalDigest(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
