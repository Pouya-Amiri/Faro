package invite

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

type Invite struct {
	Address     string
	Room        string
	Fingerprint string
	JoinToken   string
	Tailcat     string
	OwnerToken  string
}

func Parse(value string) (Invite, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return Invite{}, fmt.Errorf("parse invite: %w", err)
	}
	if parsed.Scheme != "faro" {
		return Invite{}, errors.New("invite must use the faro scheme")
	}
	if parsed.Hostname() == "" {
		return Invite{}, errors.New("invite host is required")
	}
	port := parsed.Port()
	if port == "" {
		port = "8999"
	}
	room, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil || strings.TrimSpace(room) == "" {
		return Invite{}, errors.New("invite room is required")
	}
	query := parsed.Query()
	fingerprint := query.Get("fingerprint")
	if fingerprint == "" {
		return Invite{}, errors.New("invite certificate fingerprint is required")
	}
	return Invite{
		Address: net.JoinHostPort(parsed.Hostname(), port), Room: room,
		Tailcat: query.Get("tailcat"), Fingerprint: fingerprint, JoinToken: query.Get("token"), OwnerToken: query.Get("owner"),
	}, nil
}

func Format(value Invite) (string, error) {
	if value.Address == "" || value.Room == "" || value.Fingerprint == "" {
		return "", errors.New("address, room, and fingerprint are required")
	}
	host, port, err := net.SplitHostPort(value.Address)
	if err != nil {
		return "", err
	}
	if port == "8999" {
		port = ""
	}
	query := make(url.Values)
	query.Set("fingerprint", value.Fingerprint)
	if value.Tailcat != "" {
		query.Set("tailcat", value.Tailcat)
	}
	if value.JoinToken != "" {
		query.Set("token", value.JoinToken)
	}
	if value.OwnerToken != "" {
		query.Set("owner", value.OwnerToken)
	}
	result := &url.URL{Scheme: "faro", Host: net.JoinHostPort(host, port), Path: value.Room, RawQuery: query.Encode()}
	if port == "" {
		result.Host = host
		if strings.Contains(host, ":") {
			result.Host = "[" + host + "]"
		}
	}
	return result.String(), nil
}
