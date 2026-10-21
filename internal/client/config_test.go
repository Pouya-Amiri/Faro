package client

import (
	"strings"
	"testing"
)

// Each room sees its own stable identity for an installation, so a ban
// holds there while rooms cannot link one person across them.
func TestRoomIdentityIsStablePerRoomAndUnlinkable(t *testing.T) {
	base := Config{Address: "example.test:8999", Fingerprint: strings.Repeat("a", 64), Room: "movie", Identity: "installation-secret"}
	identity := base.roomIdentity()
	if len(identity) != 43 {
		t.Fatalf("room identity %q has the wrong form", identity)
	}
	if base.roomIdentity() != identity {
		t.Fatal("the room identity changed between connections")
	}
	otherRoom, otherServer := base, base
	otherRoom.Room = "series"
	otherServer.Fingerprint = strings.Repeat("b", 64)
	if otherRoom.roomIdentity() == identity || otherServer.roomIdentity() == identity {
		t.Fatal("two rooms see the same identity")
	}
	if strings.Contains(identity, base.Identity) {
		t.Fatal("the room identity reveals the installation secret")
	}
	base.Identity = ""
	if base.roomIdentity() != "" {
		t.Fatal("an installation without a secret presented an identity")
	}
}
