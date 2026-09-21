package server

import (
	"crypto/sha256"
	"sort"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

type participant struct {
	state   protocol.Participant
	session *session
}

type room struct {
	state          protocol.Room
	ownerDigest    [sha256.Size]byte
	participants   map[string]*participant
	playback       protocol.Playback
	playlist       protocol.Playlist
	wheel          *protocol.PlaylistWheel
	wheelTimer     *time.Timer
	streamOffers   map[string]*protocol.MediaStreamOffer
	streamRequests map[string]*streamRequest
	streamGrants   map[string]*streamGrant
}

type streamRequest struct {
	id        string
	offerID   string
	provider  *participant
	viewer    *participant
	expiresAt time.Time
}

type streamGrant struct {
	requestID string
	offerID   string
	provider  *participant
	viewer    *participant
	expiresAt time.Time
}

func newRoom(id string, ownerDigest [sha256.Size]byte) *room {
	now := time.Now()
	return &room{
		state:          protocol.Room{ID: id, Mode: protocol.RoomCollaborative},
		ownerDigest:    ownerDigest,
		participants:   make(map[string]*participant),
		streamOffers:   make(map[string]*protocol.MediaStreamOffer),
		streamRequests: make(map[string]*streamRequest),
		streamGrants:   make(map[string]*streamGrant),
		playback: protocol.Playback{
			Revision:        1,
			Paused:          true,
			Rate:            1,
			UpdatedAtUnixMs: now.UnixMilli(),
		},
		playlist: protocol.Playlist{Revision: 1, Items: []protocol.PlaylistItem{}, Selected: -1},
	}
}

func (r *room) canControl(p *participant) bool {
	if r.state.Mode == protocol.RoomCollaborative {
		return true
	}
	return p != nil && (p.state.Role == protocol.RoleOwner || p.state.Role == protocol.RoleModerator)
}

func (r *room) snapshot(selfID string, includeStreamOffers ...bool) protocol.Snapshot {
	participants := make([]protocol.Participant, 0, len(r.participants))
	for _, current := range r.participants {
		state := current.state
		state.Media = cloneMedia(state.Media)
		state.AvailableMedia = append([]string(nil), state.AvailableMedia...)
		participants = append(participants, state)
	}
	sort.Slice(participants, func(i, j int) bool {
		if participants[i].Role != participants[j].Role {
			return roleRank(participants[i].Role) < roleRank(participants[j].Role)
		}
		return participants[i].Name < participants[j].Name
	})
	playback := r.playback
	playback.PositionSeconds = playback.PositionAt(time.Now())
	playback.UpdatedAtUnixMs = time.Now().UnixMilli()
	playlist := clonePlaylist(r.playlist)
	snapshot := protocol.Snapshot{SelfID: selfID, Room: r.state, Participants: participants, Playback: playback, Playlist: playlist, PlaylistWheel: cloneWheel(r.wheel)}
	if len(includeStreamOffers) > 0 && includeStreamOffers[0] {
		snapshot.StreamOffers = streamOffersOf(r)
	}
	return snapshot
}

func roleRank(role protocol.Role) int {
	switch role {
	case protocol.RoleOwner:
		return 0
	case protocol.RoleModerator:
		return 1
	default:
		return 2
	}
}
