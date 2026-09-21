package server

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Pouya-Amiri/Faro/internal/invite"
)

func TestPrintInviteRequiresHost(t *testing.T) {
	_, err := ParseConfig([]string{"--print-invite"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--invite-host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStreamingRequiresAbsoluteHTTPSDERPMap(t *testing.T) {
	for _, value := range []string{"http://relay.example/map", "/derp/map", "not a URL"} {
		if _, err := ParseConfig([]string{"--streaming-derp-map-url", value}, io.Discard); err == nil {
			t.Fatalf("accepted invalid DERP map URL %q", value)
		}
	}
	cfg, err := ParseConfig([]string{"--streaming-derp-map-url", "https://relay.example/derp/map"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StreamingEnabled {
		t.Fatal("streaming should be enabled by default")
	}
	disabled, err := ParseConfig([]string{"--streaming=false"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.StreamingEnabled {
		t.Fatal("--streaming=false did not disable streaming")
	}
}

func TestPublicAddressUsesListenPort(t *testing.T) {
	address, err := publicAddress("watch.example.net", "127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}
	if address != "watch.example.net:9443" {
		t.Fatalf("unexpected address: %s", address)
	}
}

func TestServerFormatsInviteWithoutRetainingPlaintextJoinToken(t *testing.T) {
	token := strings.Repeat("a", 32)
	cfg := DefaultConfig()
	cfg.Address = "127.0.0.1:9443"
	cfg.TLSDir = t.TempDir()
	cfg.JoinToken = token
	cfg.InviteHost = "watch.example.net"
	cfg.InviteRoom = "movie-night"
	cfg.PrintInvite = true

	service, err := New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := invite.Parse(service.InviteURL())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Address != "watch.example.net:9443" || parsed.Room != cfg.InviteRoom || parsed.JoinToken != token {
		t.Fatalf("unexpected invite: %#v", parsed)
	}
	if service.cfg.JoinToken != "" {
		t.Fatal("server retained the plaintext join token")
	}
}
