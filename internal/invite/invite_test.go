package invite

import "testing"

func TestRoundTrip(t *testing.T) {
	want := Invite{Address: "example.test:8999", Room: "Movie Night", Fingerprint: "abc", JoinToken: "join", OwnerToken: "owner"}
	encoded, err := Format(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %#v want %#v", got, want)
	}
}

func TestIPv6DefaultPortRoundTrip(t *testing.T) {
	want := Invite{Address: "[::1]:8999", Room: "watch", Fingerprint: "abc"}
	encoded, err := Format(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %#v want %#v", got, want)
	}
}

func TestPeerInviteRoundTrip(t *testing.T) {
	original := Invite{Address: "peer:8999", Room: "movie night", Fingerprint: "pinned", JoinToken: "secret", Tailcat: "tc-test-address", OwnerToken: "owner"}
	link, err := Format(original)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(link)
	if err != nil || parsed != original {
		t.Fatalf("peer invite changed: %#v, %v", parsed, err)
	}
}
