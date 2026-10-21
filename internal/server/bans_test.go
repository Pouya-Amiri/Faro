package server

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

func TestBanKeepsAnInstallationOutUntilLifted(t *testing.T) {
	h := newHub(1, 5)
	join := func(id, name, identity string) (*participant, error) {
		t.Helper()
		s := testSession(id)
		local, remote := net.Pipe()
		t.Cleanup(func() { _ = remote.Close() })
		s.conn = local
		joined, err := testJoin(h, s, protocol.Hello{Name: name, Room: "movie", ClientIdentity: identity})
		return joined.participant, err
	}
	mustJoin := func(id, name, identity string) *participant {
		t.Helper()
		p, err := join(id, name, identity)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	refused := func(err error, code string) bool {
		var command *commandError
		return errors.As(err, &command) && command.code == code
	}
	ada, ken, old := strings.Repeat("a", 43), strings.Repeat("k", 43), ""
	owner := mustJoin("owner", "Ada", ada)
	member := mustJoin("member", "Ken", ken)
	legacy := mustJoin("legacy", "Old", old)

	if err := h.kick(owner, protocol.RoomParticipantKick{ParticipantID: legacy.state.ID, Ban: true}); !refused(err, "ban_unsupported") {
		t.Fatalf("banning a client without an identity returned %v", err)
	}
	if h.rooms["movie"].participants[legacy.state.ID] == nil {
		t.Fatal("a ban that could not be kept still removed the participant")
	}
	if err := h.kick(owner, protocol.RoomParticipantKick{ParticipantID: member.state.ID, Ban: true}); err != nil {
		t.Fatal(err)
	}
	if h.rooms["movie"].participants[member.state.ID] != nil {
		t.Fatal("the banned participant is still in the room")
	}
	bans := h.rooms["movie"].state.Bans
	if len(bans) != 1 || bans[0].Name != "Ken" {
		t.Fatalf("room bans = %+v", bans)
	}
	if _, err := join("member-again", "Someone else", ken); !refused(err, protocol.ErrorBanned) {
		t.Fatalf("a banned installation rejoined under another name: %v", err)
	}
	if _, err := join("bad", "Bad", "short"); !refused(err, "invalid_hello") {
		t.Fatalf("a malformed identity was accepted: %v", err)
	}

	// The room closing does not end the ban: whoever reopened it would own it.
	h.leave(owner)
	h.leave(legacy)
	if h.rooms["movie"] != nil {
		t.Fatal("the empty room stayed open")
	}
	if _, err := join("member-reopen", "Ken", ken); !refused(err, protocol.ErrorBanned) {
		t.Fatalf("a banned installation reopened the room: %v", err)
	}
	owner = mustJoin("owner-again", "Ada", ada)
	if bans := h.rooms["movie"].state.Bans; len(bans) != 1 {
		t.Fatalf("the reopened room lists bans %+v", bans)
	}

	if err := h.liftBan(owner, protocol.RoomBanLift{BanID: bans[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := h.liftBan(owner, protocol.RoomBanLift{BanID: bans[0].ID}); !refused(err, "ban_not_found") {
		t.Fatalf("lifting a lifted ban returned %v", err)
	}
	if h.rooms["movie"].state.Bans != nil {
		t.Fatalf("a lifted ban is still listed: %+v", h.rooms["movie"].state.Bans)
	}
	mustJoin("member-forgiven", "Ken", ken)
}

func TestModeratorsBanOnlyMembersAndMembersCannotLiftBans(t *testing.T) {
	h := newHub(1, 5)
	join := func(id, identity string) *participant {
		t.Helper()
		s := testSession(id)
		local, remote := net.Pipe()
		t.Cleanup(func() { _ = remote.Close() })
		s.conn = local
		joined, err := testJoin(h, s, protocol.Hello{Name: id, Room: "movie", ClientIdentity: strings.Repeat(identity, 43)})
		if err != nil {
			t.Fatal(err)
		}
		return joined.participant
	}
	owner, moderator, other, member, watcher := join("owner", "o"), join("moderator", "m"), join("other", "x"), join("member", "e"), join("watcher", "w")
	for _, p := range []*participant{moderator, other} {
		if err := h.setRole(owner, protocol.RoomRoleSet{ParticipantID: p.state.ID, Role: protocol.RoleModerator}); err != nil {
			t.Fatal(err)
		}
	}
	for _, denied := range []struct{ actor, target *participant }{{member, watcher}, {moderator, other}, {moderator, owner}} {
		if err := h.kick(denied.actor, protocol.RoomParticipantKick{ParticipantID: denied.target.state.ID, Ban: true}); err == nil {
			t.Fatalf("%s banned %s", denied.actor.state.Name, denied.target.state.Name)
		}
	}
	if len(h.bans) != 0 {
		t.Fatalf("a refused ban was recorded: %+v", h.bans)
	}
	if err := h.kick(moderator, protocol.RoomParticipantKick{ParticipantID: member.state.ID, Ban: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.liftBan(watcher, protocol.RoomBanLift{BanID: h.rooms["movie"].state.Bans[0].ID}); err == nil {
		t.Fatal("a member lifted a ban")
	}
}

func TestClosedRoomsKeepABoundedNumberOfBanLists(t *testing.T) {
	h := newHub(1, 5)
	for index := range maxRetainedBanLists + 5 {
		room := strings.Repeat("r", index+1)
		owner, member := testSession("owner"), testSession("member")
		for _, s := range []*session{owner, member} {
			local, remote := net.Pipe()
			t.Cleanup(func() { _ = remote.Close() })
			s.conn = local
		}
		if _, err := testJoin(h, owner, protocol.Hello{Name: "owner", Room: room, ClientIdentity: strings.Repeat("o", 43)}); err != nil {
			t.Fatal(err)
		}
		if _, err := testJoin(h, member, protocol.Hello{Name: "member", Room: room, ClientIdentity: strings.Repeat("m", 43)}); err != nil {
			t.Fatal(err)
		}
		if err := h.kick(owner.participant, protocol.RoomParticipantKick{ParticipantID: member.participant.state.ID, Ban: true}); err != nil {
			t.Fatal(err)
		}
		h.leave(owner.participant)
	}
	if len(h.bans) != h.maxRooms+maxRetainedBanLists {
		t.Fatalf("%d rooms keep bans, want %d", len(h.bans), h.maxRooms+maxRetainedBanLists)
	}
	if _, kept := h.bans[strings.Repeat("r", maxRetainedBanLists+5)]; !kept {
		t.Fatal("the newest ban list was dropped")
	}
}
