package client

import (
	"context"
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
