package server

import (
	"crypto/sha256"
	"regexp"
	"slices"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const (
	// maxRoomBans bounds one room's ban list.
	maxRoomBans = 100
	// maxRetainedBanLists bounds how many rooms that have closed keep their
	// bans, beyond the rooms that are open.
	maxRetainedBanLists = 64
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

// banEntry is a ban and the digest of the identity it refuses.
type banEntry struct {
	ban      protocol.RoomBan
	identity [sha256.Size]byte
}

// clientIdentity validates the identity a client presented and returns its
// digest; ok is false for a client that presented none.
func clientIdentity(value string) (digest [sha256.Size]byte, ok bool, err error) {
	if value == "" {
		return digest, false, nil
	}
	if !identityPattern.MatchString(value) {
		return digest, false, invalid("invalid_hello", "client identity is invalid")
	}
	return sha256.Sum256([]byte(value)), true, nil
}

// bannedLocked reports whether identity is banned from the room roomID.
func (h *hub) bannedLocked(roomID string, identity [sha256.Size]byte) bool {
	return slices.ContainsFunc(h.bans[roomID], func(entry banEntry) bool { return entry.identity == identity })
}

// banLocked keeps target out of r. A room's bans are kept by its ID, not
// with the room: rooms close when their last participant leaves, and a
// banned person must not be able to open it again.
func (h *hub) banLocked(r *room, target *participant) error {
	if !target.identified {
		return invalid("ban_unsupported", target.state.Name+"'s version of Faro can't be banned; remove them instead")
	}
	if h.bannedLocked(r.state.ID, target.identity) {
		return nil
	}
	entries := h.bans[r.state.ID]
	if len(entries) >= maxRoomBans {
		return invalid("too_many_bans", "this room has banned as many people as it can; lift a ban first")
	}
	id, err := randomToken()
	if err != nil {
		return err
	}
	if entries == nil {
		h.retainBanListLocked()
	}
	h.bans[r.state.ID] = append(entries, banEntry{
		ban:      protocol.RoomBan{ID: id, Name: target.state.Name, BannedAtUnixMs: time.Now().UnixMilli()},
		identity: target.identity,
	})
	h.syncBansLocked(r)
	return nil
}

// retainBanListLocked makes room for one more room's bans by forgetting
// those of the closed room banned from longest ago. Open rooms are never
// more than maxRooms, so a closed one is always found when the limit is
// reached.
func (h *hub) retainBanListLocked() {
	if len(h.bans) < h.maxRooms+maxRetainedBanLists {
		return
	}
	oldest, at := "", int64(0)
	for id, entries := range h.bans {
		if h.rooms[id] != nil {
			continue
		}
		latest := entries[len(entries)-1].ban.BannedAtUnixMs
		if oldest == "" || latest < at {
			oldest, at = id, latest
		}
	}
	delete(h.bans, oldest)
}

// syncBansLocked publishes r's bans in its state.
func (h *hub) syncBansLocked(r *room) {
	entries := h.bans[r.state.ID]
	r.state.Bans = make([]protocol.RoomBan, 0, len(entries))
	for _, entry := range entries {
		r.state.Bans = append(r.state.Bans, entry.ban)
	}
	if len(r.state.Bans) == 0 {
		r.state.Bans = nil
	}
}

// liftBan lets a banned person join p's room again. Owners and moderators
// can lift bans.
func (h *hub) liftBan(p *participant, request protocol.RoomBanLift) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.roomForLocked(p)
	if r == nil || p.state.Role != protocol.RoleOwner && p.state.Role != protocol.RoleModerator {
		return invalid("forbidden", "only owners and moderators can lift bans")
	}
	entries := h.bans[r.state.ID]
	index := slices.IndexFunc(entries, func(entry banEntry) bool { return entry.ban.ID == request.BanID })
	if index < 0 {
		return invalid("ban_not_found", "that ban was already lifted")
	}
	if entries = slices.Delete(entries, index, index+1); len(entries) == 0 {
		delete(h.bans, r.state.ID)
	} else {
		h.bans[r.state.ID] = entries
	}
	h.syncBansLocked(r)
	broadcast(sessionsOf(r), protocol.TypeRoomUpdated, r.state)
	return nil
}
