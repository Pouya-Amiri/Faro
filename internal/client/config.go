package client

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

type Config struct {
	DialContext   func(context.Context, string, string) (net.Conn, error)
	Address       string
	ServerName    string
	Fingerprint   string
	JoinToken     string
	OwnerToken    string
	Name          string
	Room          string
	ClientVersion string
	Capabilities  []protocol.Capability
	// Identity is this installation's secret. Each room receives its own
	// value derived from it, so rooms cannot link one person across them.
	Identity string
}

// roomIdentity derives what identifies this installation to one room of one
// server.
func (c Config) roomIdentity() string {
	if c.Identity == "" {
		return ""
	}
	server := c.Fingerprint
	if server == "" {
		server = c.Address
	}
	mac := hmac.New(sha256.New, []byte(c.Identity))
	mac.Write([]byte("faro room identity v1\x00" + server + "\x00" + c.Room))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return fmt.Errorf("invalid server address %q: %w", c.Address, err)
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(c.Room) == "" {
		return errors.New("room is required")
	}
	if c.Fingerprint == "" && c.ServerName == "" {
		return errors.New("a certificate fingerprint or server name is required")
	}
	if c.ClientVersion == "" {
		return errors.New("client version is required")
	}
	if len(c.Capabilities) > 32 {
		return errors.New("too many client capabilities")
	}
	seen := make(map[protocol.Capability]struct{}, len(c.Capabilities))
	for _, capability := range c.Capabilities {
		if capability == "" || len(capability) > 64 || strings.ContainsAny(string(capability), "\r\n\x00") {
			return errors.New("client capability is invalid")
		}
		if _, duplicate := seen[capability]; duplicate {
			return errors.New("client capabilities must be unique")
		}
		seen[capability] = struct{}{}
	}
	return nil
}
