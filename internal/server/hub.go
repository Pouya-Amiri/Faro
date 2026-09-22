package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const (
	maxNameRunes           = 64
	maxRoomRunes           = 128
	maxChatRunes           = 2000
	maxPlaylistItems       = 500
	maxPlaylistText        = 64 * 1024
	maxMediaTitleRunes     = 256
	maxOfferIDBytes        = 128
	maxPublicKeyBytes      = 2048
	maxConnectionBlobBytes = 64 * 1024
	maxRevokeReasonRunes   = 256
	wheelStartDelay        = 700 * time.Millisecond
	wheelDuration          = 4800 * time.Millisecond
)

type commandError struct {
	code    string
	message string
}

func (e *commandError) Error() string { return e.message }

func invalid(code, message string) error { return &commandError{code: code, message: message} }

type hub struct {
	mu                  sync.Mutex
	rooms               map[string]*room
	maxRooms            int
	maxRoomParticipants int
	streaming           *protocol.MediaStreamPolicy
}

type joinResult struct {
	participant *participant
	room        *room
	ownerToken  string
	existing    []*session
}

func newHub(maxRooms, maxRoomParticipants int, streaming ...*protocol.MediaStreamPolicy) *hub {
	var policy *protocol.MediaStreamPolicy
	if len(streaming) > 0 && streaming[0] != nil {
		copy := *streaming[0]
		policy = &copy
	}
	return &hub{rooms: make(map[string]*room), maxRooms: maxRooms, maxRoomParticipants: maxRoomParticipants, streaming: policy}
}

func (h *hub) join(s *session, hello protocol.Hello) (joinResult, error) {
	name, err := validateText("name", hello.Name, maxNameRunes)
	if err != nil {
		return joinResult{}, err
	}
	roomID, err := validateText("room", hello.Room, maxRoomRunes)
	if err != nil {
		return joinResult{}, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.rooms[roomID]
	ownerToken := ""
	if r == nil {
		if len(h.rooms) >= h.maxRooms {
			return joinResult{}, invalid("server_full", "the server has reached its active room limit")
		}
		ownerToken, err = randomToken()
		if err != nil {
			return joinResult{}, fmt.Errorf("generate owner capability: %w", err)
		}
		r = newRoom(roomID, sha256.Sum256([]byte(ownerToken)))
		h.rooms[roomID] = r
	}
	if len(r.participants) >= h.maxRoomParticipants {
		return joinResult{}, invalid("room_full", "the room has reached its participant limit")
	}
	role := protocol.RoleMember
	if ownerToken != "" {
		role = protocol.RoleOwner
	} else if hello.OwnerToken != "" {
		digest := sha256.Sum256([]byte(hello.OwnerToken))
		if !hmac.Equal(digest[:], r.ownerDigest[:]) {
			return joinResult{}, invalid("invalid_owner_token", "the room owner capability is invalid")
		}
		role = protocol.RoleOwner
	}
	name = freeName(name, r.participants)
	p := &participant{
		state:   protocol.Participant{ID: s.id, Name: name, Role: role},
		session: s,
	}
	r.participants[p.state.ID] = p
	return joinResult{participant: p, room: r, ownerToken: ownerToken, existing: sessionsOf(r)}, nil
}

func (h *hub) leave(p *participant) {
	if p == nil {
		return
	}
	h.mu.Lock()
	var recipients []*session
	var participants []protocol.Participant
	var cancelled *protocol.PlaylistWheel
	var streamRecipients []*session
	var streamOffers []protocol.MediaStreamOffer
	var revocations []directedRevocation
	for id, r := range h.rooms {
		if r.participants[p.state.ID] == nil {
			continue
		}
		delete(r.participants, p.state.ID)
		revocations = append(revocations, removeParticipantStreamsLocked(r, p, "participant left the room")...)
		if r.wheel != nil && r.wheel.RequesterID == p.state.ID {
			cancelled = cancelWheelLocked(r, "The person who spun the wheel left the room")
		}
		if len(r.participants) == 0 {
			delete(h.rooms, id)
		} else {
			ensureModerator(r)
			recipients = sessionsOf(r)
			participants = participantsOf(r)
			streamRecipients = streamSessionsOf(r)
			streamOffers = streamOffersOf(r)
		}
		break
	}
	h.mu.Unlock()
	if cancelled != nil {
		broadcast(recipients, protocol.TypePlaylistWheelUpdated, *cancelled)
	}
	broadcast(recipients, protocol.TypeParticipantsUpdated, participants)
	broadcast(streamRecipients, protocol.TypeStreamOffersUpdated, streamOffers)
	sendRevocations(revocations)
}

func ensureModerator(r *room) {
	if r.state.Mode != protocol.RoomModerated {
		return
	}
	for _, current := range r.participants {
		if current.state.Role == protocol.RoleOwner || current.state.Role == protocol.RoleModerator {
			return
		}
	}
	var selected *participant
	for _, current := range r.participants {
		if selected == nil || current.state.Name < selected.state.Name {
			selected = current
		}
	}
	if selected != nil {
		selected.state.Role = protocol.RoleModerator
	}
}

func (h *hub) snapshot(p *participant) (protocol.Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.roomForLocked(p)
	if r == nil {
		return protocol.Snapshot{}, invalid("not_joined", "the participant is not in a room")
	}
	return r.snapshot(p.state.ID, p.session.supports(protocol.CapabilityMediaStreamV1)), nil
}

func (h *hub) participants(p *participant) ([]protocol.Participant, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.roomForLocked(p)
	if r == nil {
		return nil, invalid("not_joined", "the participant is not in a room")
	}
	return participantsOf(r), nil
}

func (h *hub) setMode(p *participant, request protocol.RoomModeSet) error {
	if request.Mode != protocol.RoomCollaborative && request.Mode != protocol.RoomModerated {
		return invalid("invalid_room_mode", "room mode must be collaborative or moderated")
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil || p.state.Role != protocol.RoleOwner {
		h.mu.Unlock()
		return invalid("forbidden", "only a room owner can change the room mode")
	}
	r.state.Mode = request.Mode
	recipients, state := sessionsOf(r), r.state
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeRoomUpdated, state)
	return nil
}

func (h *hub) setRole(p *participant, request protocol.RoomRoleSet) error {
	if request.Role != protocol.RoleModerator && request.Role != protocol.RoleMember {
		return invalid("invalid_role", "a participant can be a moderator or member")
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil || p.state.Role != protocol.RoleOwner {
		h.mu.Unlock()
		return invalid("forbidden", "only a room owner can change roles")
	}
	target := r.participants[request.ParticipantID]
	if target == nil {
		h.mu.Unlock()
		return invalid("participant_not_found", "participant not found")
	}
	if target.state.Role == protocol.RoleOwner {
		h.mu.Unlock()
		return invalid("forbidden", "owner capabilities cannot be changed through roles")
	}
	target.state.Role = request.Role
	recipients, participants := sessionsOf(r), participantsOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeParticipantsUpdated, participants)
	return nil
}

func (h *hub) setPlayback(p *participant, request protocol.PlaybackSet) error {
	if math.IsNaN(request.PositionSeconds) || math.IsInf(request.PositionSeconds, 0) || request.PositionSeconds < 0 {
		return invalid("invalid_position", "playback position must be non-negative")
	}
	if math.IsNaN(request.Rate) || math.IsInf(request.Rate, 0) || request.Rate < 0.25 || request.Rate > 4 {
		return invalid("invalid_rate", "playback rate must be between 0.25 and 4")
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil || !r.canControl(p) {
		h.mu.Unlock()
		return invalid("forbidden", "this participant cannot control playback")
	}
	previous := r.playback
	now := time.Now()
	r.playback.Revision++
	r.playback.PositionSeconds = request.PositionSeconds
	r.playback.Paused = request.Paused
	r.playback.Rate = request.Rate
	r.playback.UpdatedAtUnixMs = now.UnixMilli()
	r.playback.SetBy = p.state.ID
	playback := r.playback
	r.playback.Seek = request.Seek
	playback.Seek = request.Seek
	recipients := sessionsOf(r)
	participantID, participantName := p.state.ID, p.state.Name
	h.mu.Unlock()
	broadcast(recipients, protocol.TypePlaybackUpdated, playback)
	var action protocol.ActivityAction
	switch {
	case request.Seek && request.SponsorBlock:
		action = protocol.ActivitySponsorSkipped
	case request.Seek:
		action = protocol.ActivityPlaybackSeeked
	case request.Paused != previous.Paused && request.Paused:
		action = protocol.ActivityPlaybackPaused
	case request.Paused != previous.Paused:
		action = protocol.ActivityPlaybackResumed
	case request.Rate != previous.Rate:
		action = protocol.ActivityPlaybackRate
	}
	if action != "" {
		activity := activityMessage(participantID, participantName, action, request.PositionSeconds, request.Rate, "", 0)
		if action == protocol.ActivitySponsorSkipped {
			activity.ParticipantID = ""
			activity.ParticipantName = ""
		}
		if action == protocol.ActivityPlaybackSeeked {
			activity.DeltaSeconds = request.PositionSeconds - previous.PositionAt(now)
		}
		broadcast(recipients, protocol.TypeActivityMessage, activity)
	}
	return nil
}

func (h *hub) setMedia(p *participant, request protocol.MediaSet) error {
	if request.Media != nil {
		request.Media.Title = strings.TrimSpace(request.Media.Title)
		if request.Media.Title == "" || utf8.RuneCountInString(request.Media.Title) > maxMediaTitleRunes {
			return invalid("invalid_media", "media title is required and must not exceed 256 characters")
		}
		if request.Media.DurationSeconds < 0 || math.IsNaN(request.Media.DurationSeconds) || math.IsInf(request.Media.DurationSeconds, 0) {
			return invalid("invalid_media", "media duration must be non-negative")
		}
		if len(request.Media.Fingerprint) > 128 {
			return invalid("invalid_media", "media fingerprint is too long")
		}
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return invalid("not_joined", "the participant is not in a room")
	}
	p.state.Media = cloneMedia(request.Media)
	recipients, participants := sessionsOf(r), participantsOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeMediaUpdated, participants)
	return nil
}

func (h *hub) setPlaylist(p *participant, request protocol.PlaylistSet) error {
	items, err := validatePlaylist(request.Items)
	if err != nil {
		return err
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil || !r.canControl(p) {
		h.mu.Unlock()
		return invalid("forbidden", "this participant cannot change the playlist")
	}
	cancelled := cancelWheelLocked(r, "The queue changed before the spin finished")
	selectedID := ""
	var previousItem *protocol.PlaylistItem
	if r.playlist.Selected >= 0 && r.playlist.Selected < len(r.playlist.Items) {
		item := r.playlist.Items[r.playlist.Selected]
		selectedID = item.ID
		previousItem = &item
	}
	r.playlist.Revision++
	r.playlist.Items = items
	r.playlist.Selected = -1
	if selectedID != "" {
		for index := range items {
			if items[index].ID == selectedID {
				r.playlist.Selected = index
				break
			}
		}
	}
	selectionChanged := previousItem != nil && (r.playlist.Selected < 0 || !samePlaylistSource(*previousItem, items[r.playlist.Selected]))
	var playback protocol.Playback
	if selectionChanged {
		r.playback.Revision++
		r.playback.PositionSeconds = 0
		r.playback.Paused = true
		r.playback.Rate = 1
		r.playback.UpdatedAtUnixMs = time.Now().UnixMilli()
		r.playback.SetBy = p.state.ID
		r.playback.Seek = true
		playback = r.playback
	}
	recipients, playlist := sessionsOf(r), clonePlaylist(r.playlist)
	participantID, participantName := p.state.ID, p.state.Name
	h.mu.Unlock()
	if cancelled != nil {
		broadcast(recipients, protocol.TypePlaylistWheelUpdated, *cancelled)
	}
	broadcast(recipients, protocol.TypePlaylistUpdated, playlist)
	if selectionChanged {
		broadcast(recipients, protocol.TypePlaybackUpdated, playback)
	}
	broadcast(recipients, protocol.TypeActivityMessage, activityMessage(participantID, participantName, protocol.ActivityPlaylistUpdated, 0, 0, "", len(playlist.Items)))
	return nil
}

func samePlaylistSource(a, b protocol.PlaylistItem) bool {
	if a.URL != "" || b.URL != "" {
		return a.URL == b.URL
	}
	return a.Media != nil && b.Media != nil && a.Media.Fingerprint == b.Media.Fingerprint
}

func (h *hub) selectPlaylist(p *participant, index int) error {
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil || !r.canControl(p) {
		h.mu.Unlock()
		return invalid("forbidden", "this participant cannot select playlist items")
	}
	if index < -1 || index >= len(r.playlist.Items) {
		h.mu.Unlock()
		return invalid("invalid_playlist_index", "playlist index is out of range")
	}
	cancelled := cancelWheelLocked(r, "Another queue item was selected")
	r.playlist.Revision++
	r.playlist.Selected = index
	r.playback.Revision++
	r.playback.PositionSeconds = 0
	r.playback.Paused = true
	r.playback.Rate = 1
	r.playback.UpdatedAtUnixMs = time.Now().UnixMilli()
	r.playback.SetBy = p.state.ID
	playback := r.playback
	r.playback.Seek = true
	playback.Seek = true
	recipients, playlist := sessionsOf(r), clonePlaylist(r.playlist)
	selectedLabel := ""
	if index >= 0 {
		selectedLabel = playlist.Items[index].Label
	}
	participantID, participantName := p.state.ID, p.state.Name
	h.mu.Unlock()
	if cancelled != nil {
		broadcast(recipients, protocol.TypePlaylistWheelUpdated, *cancelled)
	}
	broadcast(recipients, protocol.TypePlaylistUpdated, playlist)
	broadcast(recipients, protocol.TypePlaybackUpdated, playback)
	if selectedLabel != "" {
		broadcast(recipients, protocol.TypeActivityMessage, activityMessage(participantID, participantName, protocol.ActivityPlaylistPlayed, 0, 0, selectedLabel, 0))
	}
	return nil
}

func (h *hub) spinPlaylistWheel(p *participant) error {
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil || !r.canControl(p) {
		h.mu.Unlock()
		return invalid("forbidden", "this participant cannot spin the playlist wheel")
	}
	if len(r.playlist.Items) < 2 {
		h.mu.Unlock()
		return invalid("wheel_needs_items", "add at least two queue items before spinning the wheel")
	}
	if r.wheel != nil {
		h.mu.Unlock()
		return invalid("wheel_active", "the playlist wheel is already spinning")
	}
	id, err := randomToken()
	if err != nil {
		h.mu.Unlock()
		return err
	}
	winner, err := secureRandomInt(len(r.playlist.Items))
	if err != nil {
		h.mu.Unlock()
		return err
	}
	extraTurns, err := secureRandomInt(4)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	visualSeed, err := secureRandomUint64()
	if err != nil {
		h.mu.Unlock()
		return err
	}
	items := make([]protocol.PlaylistWheelItem, len(r.playlist.Items))
	for index, item := range r.playlist.Items {
		items[index] = protocol.PlaylistWheelItem{ID: item.ID, Label: item.Label}
	}
	now := time.Now()
	wheel := &protocol.PlaylistWheel{
		ID: id, Phase: protocol.PlaylistWheelStarted,
		RequesterID: p.state.ID, RequesterName: p.state.Name,
		PlaylistRevision: r.playlist.Revision, Items: items,
		Winner: winner, Turns: 7 + extraTurns, VisualSeed: visualSeed,
		StartsAtUnixMs: now.Add(wheelStartDelay).UnixMilli(), DurationMs: int(wheelDuration.Milliseconds()),
	}
	r.wheel = cloneWheel(wheel)
	recipients := sessionsOf(r)
	roomID := r.state.ID
	participantID, participantName := p.state.ID, p.state.Name
	r.wheelTimer = time.AfterFunc(wheelStartDelay+wheelDuration, func() { h.finishPlaylistWheel(roomID, id) })
	h.mu.Unlock()
	broadcast(recipients, protocol.TypePlaylistWheelUpdated, *wheel)
	broadcast(recipients, protocol.TypeActivityMessage, activityMessage(participantID, participantName, protocol.ActivityWheelStarted, 0, 0, "", 0))
	return nil
}

type directedRevocation struct {
	recipient *session
	payload   protocol.MediaStreamRevoked
}

func (h *hub) publishStreamOffer(p *participant, request protocol.MediaStreamOfferPublish) error {
	if h.streaming == nil || p == nil || !p.session.supports(protocol.CapabilityMediaStreamV1) {
		return invalid("streaming_unavailable", "media streaming is not available for this session")
	}
	request.OfferID = strings.TrimSpace(request.OfferID)
	request.Media.Title = strings.TrimSpace(request.Media.Title)
	if request.OfferID == "" || len(request.OfferID) > maxOfferIDBytes || strings.ContainsAny(request.OfferID, "\r\n\x00") {
		return invalid("invalid_stream_offer", "stream offer ID is invalid")
	}
	if err := validateStreamMedia(request.Media); err != nil {
		return err
	}
	maxViewers := request.MaxViewers
	if maxViewers == 0 {
		maxViewers = h.streaming.MaxViewersPerOffer
	}
	if maxViewers < 1 || maxViewers > h.streaming.MaxViewersPerOffer {
		return invalid("invalid_stream_offer", "stream offer viewer limit is invalid")
	}

	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return invalid("not_joined", "the participant is not in a room")
	}
	existing := r.streamOffers[request.OfferID]
	if existing != nil && existing.ProviderID != p.state.ID {
		h.mu.Unlock()
		return invalid("stream_offer_exists", "stream offer ID is already in use")
	}
	if existing == nil {
		count := 0
		for _, offer := range r.streamOffers {
			if offer.ProviderID == p.state.ID {
				count++
			}
		}
		if count >= h.streaming.MaxOffersPerParticipant {
			h.mu.Unlock()
			return invalid("stream_offer_limit", "participant has reached the stream offer limit")
		}
		existing = &protocol.MediaStreamOffer{ID: request.OfferID, ProviderID: p.state.ID, ProviderName: p.state.Name, Revision: 1}
		r.streamOffers[request.OfferID] = existing
	} else {
		if offerHasTransfersLocked(r, existing.ID) {
			h.mu.Unlock()
			return invalid("stream_offer_active", "an active stream offer cannot change media or limits")
		}
		existing.Revision++
	}
	existing.Media = request.Media
	existing.MaxViewers = maxViewers
	recipients, offers := streamSessionsOf(r), streamOffersOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeStreamOffersUpdated, offers)
	return nil
}

func (h *hub) withdrawStreamOffer(p *participant, offerID string) error {
	offerID = strings.TrimSpace(offerID)
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return invalid("not_joined", "the participant is not in a room")
	}
	offer := r.streamOffers[offerID]
	if offer == nil {
		h.mu.Unlock()
		return invalid("stream_offer_not_found", "stream offer not found")
	}
	if offer.ProviderID != p.state.ID {
		h.mu.Unlock()
		return invalid("forbidden", "only the provider can withdraw a stream offer")
	}
	revocations := removeOfferStreamsLocked(r, offerID, "provider withdrew the stream offer")
	delete(r.streamOffers, offerID)
	recipients, offers := streamSessionsOf(r), streamOffersOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeStreamOffersUpdated, offers)
	sendRevocations(revocations)
	return nil
}

func (h *hub) requestStream(p *participant, request protocol.MediaStreamRequest) (protocol.MediaStreamRequestAccepted, error) {
	if h.streaming == nil || p == nil || !p.session.supports(protocol.CapabilityMediaStreamV1) {
		return protocol.MediaStreamRequestAccepted{}, invalid("streaming_unavailable", "media streaming is not available for this session")
	}
	request.ClientPublicKey = strings.TrimSpace(request.ClientPublicKey)
	if request.ClientPublicKey == "" || len(request.ClientPublicKey) > maxPublicKeyBytes || strings.ContainsAny(request.ClientPublicKey, "\r\n\x00") {
		return protocol.MediaStreamRequestAccepted{}, invalid("invalid_stream_request", "Tailcat client public key is invalid")
	}
	requestID, err := randomToken()
	if err != nil {
		return protocol.MediaStreamRequestAccepted{}, err
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(h.streaming.GrantTTLSeconds) * time.Second)
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return protocol.MediaStreamRequestAccepted{}, invalid("not_joined", "the participant is not in a room")
	}
	offer := r.streamOffers[request.OfferID]
	if offer == nil {
		h.mu.Unlock()
		return protocol.MediaStreamRequestAccepted{}, invalid("stream_offer_not_found", "stream offer not found")
	}
	provider := r.participants[offer.ProviderID]
	if provider == nil || !provider.session.supports(protocol.CapabilityMediaStreamV1) {
		h.mu.Unlock()
		return protocol.MediaStreamRequestAccepted{}, invalid("stream_provider_unavailable", "stream provider is unavailable")
	}
	if provider == p {
		h.mu.Unlock()
		return protocol.MediaStreamRequestAccepted{}, invalid("invalid_stream_request", "a provider cannot request its own stream")
	}
	active := 0
	for _, pending := range r.streamRequests {
		if pending.offerID == offer.ID {
			active++
		}
		pendingOffer := r.streamOffers[pending.offerID]
		if pending.viewer == p && pendingOffer != nil && pendingOffer.Media.Fingerprint == offer.Media.Fingerprint {
			h.mu.Unlock()
			return protocol.MediaStreamRequestAccepted{}, invalid("stream_request_exists", "viewer already requested a provider for this media")
		}
	}
	for _, grant := range r.streamGrants {
		if grant.offerID == offer.ID {
			active++
		}
		grantedOffer := r.streamOffers[grant.offerID]
		if grant.viewer == p && grantedOffer != nil && grantedOffer.Media.Fingerprint == offer.Media.Fingerprint {
			h.mu.Unlock()
			return protocol.MediaStreamRequestAccepted{}, invalid("stream_grant_exists", "viewer already selected a provider for this media")
		}
	}
	if active >= offer.MaxViewers {
		h.mu.Unlock()
		return protocol.MediaStreamRequestAccepted{}, invalid("stream_offer_full", "stream offer has reached its viewer limit")
	}
	r.streamRequests[requestID] = &streamRequest{id: requestID, offerID: offer.ID, provider: provider, viewer: p, expiresAt: expiresAt}
	roomID := r.state.ID
	h.mu.Unlock()
	provider.session.sendMessage(protocol.TypeStreamRequested, "", "", protocol.MediaStreamRequested{
		RequestID: requestID, OfferID: offer.ID, ViewerID: p.state.ID, ViewerName: p.state.Name,
		ClientPublicKey: request.ClientPublicKey, ExpiresAtUnixMs: expiresAt.UnixMilli(),
	})
	time.AfterFunc(time.Until(expiresAt), func() { h.expireStreamRequest(roomID, requestID) })
	return protocol.MediaStreamRequestAccepted{RequestID: requestID, OfferID: offer.ID, ExpiresAtUnixMs: expiresAt.UnixMilli()}, nil
}

func (h *hub) grantStream(p *participant, grant protocol.MediaStreamGrant) error {
	if h.streaming == nil || p == nil || !p.session.supports(protocol.CapabilityMediaStreamV1) {
		return invalid("streaming_unavailable", "media streaming is not available for this session")
	}
	if len(grant.ConnectionBlob) == 0 || len(grant.ConnectionBlob) > maxConnectionBlobBytes {
		return invalid("invalid_stream_grant", "stream connection blob is invalid")
	}
	if !tokenPattern.MatchString(grant.TransferCapability) {
		return invalid("invalid_stream_grant", "stream transfer capability is invalid")
	}
	now := time.Now()
	expiresAt := time.UnixMilli(grant.ExpiresAtUnixMs)
	maximumExpiry := now.Add(time.Duration(h.streaming.GrantTTLSeconds) * time.Second)
	if !expiresAt.After(now) || expiresAt.After(maximumExpiry) {
		return invalid("invalid_stream_grant", "stream grant expiry is invalid")
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	request := (*streamRequest)(nil)
	if r != nil {
		request = r.streamRequests[grant.RequestID]
	}
	if request == nil || now.After(request.expiresAt) {
		h.mu.Unlock()
		return invalid("stream_request_not_found", "stream request is missing or expired")
	}
	if request.provider != p {
		h.mu.Unlock()
		return invalid("forbidden", "only the requested provider can grant this stream")
	}
	if h.roomForLocked(request.viewer) != r {
		delete(r.streamRequests, request.id)
		h.mu.Unlock()
		return invalid("stream_viewer_unavailable", "stream viewer is no longer in the room")
	}
	offer := r.streamOffers[request.offerID]
	if offer == nil || offer.ProviderID != p.state.ID {
		delete(r.streamRequests, request.id)
		h.mu.Unlock()
		return invalid("stream_offer_not_found", "stream offer is no longer available")
	}
	delete(r.streamRequests, request.id)
	r.streamGrants[request.id] = &streamGrant{requestID: request.id, offerID: request.offerID, provider: p, viewer: request.viewer, expiresAt: expiresAt}
	offer.ViewerCount++
	viewer, providerName := request.viewer.session, p.state.Name
	recipients, offers, roomID := streamSessionsOf(r), streamOffersOf(r), r.state.ID
	h.mu.Unlock()
	viewer.sendMessage(protocol.TypeStreamGranted, "", "", protocol.MediaStreamGranted{
		RequestID: request.id, OfferID: request.offerID, ProviderID: p.state.ID, ProviderName: providerName,
		ConnectionBlob: grant.ConnectionBlob, TransferCapability: grant.TransferCapability, ExpiresAtUnixMs: grant.ExpiresAtUnixMs,
	})
	broadcast(recipients, protocol.TypeStreamOffersUpdated, offers)
	time.AfterFunc(time.Until(expiresAt), func() { h.expireStreamGrant(roomID, request.id) })
	return nil
}

func (h *hub) revokeStream(p *participant, request protocol.MediaStreamRevoke) error {
	reason := strings.TrimSpace(request.Reason)
	if utf8.RuneCountInString(reason) > maxRevokeReasonRunes || strings.ContainsAny(reason, "\r\n\x00") {
		return invalid("invalid_stream_revoke", "stream revocation reason is invalid")
	}
	if reason == "" {
		reason = "stream access was revoked"
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return invalid("not_joined", "the participant is not in a room")
	}
	var revocations []directedRevocation
	if pending := r.streamRequests[request.RequestID]; pending != nil {
		if pending.provider != p && pending.viewer != p {
			h.mu.Unlock()
			return invalid("forbidden", "only the provider or viewer can revoke this stream request")
		}
		delete(r.streamRequests, request.RequestID)
		revocations = revocationsFor(pending.provider, pending.viewer, pending.id, pending.offerID, reason)
	} else if grant := r.streamGrants[request.RequestID]; grant != nil {
		if grant.provider != p && grant.viewer != p {
			h.mu.Unlock()
			return invalid("forbidden", "only the provider or viewer can revoke this stream grant")
		}
		delete(r.streamGrants, request.RequestID)
		decrementOfferViewers(r, grant.offerID)
		revocations = revocationsFor(grant.provider, grant.viewer, grant.requestID, grant.offerID, reason)
	} else {
		h.mu.Unlock()
		return invalid("stream_request_not_found", "stream request or grant not found")
	}
	recipients, offers := streamSessionsOf(r), streamOffersOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeStreamOffersUpdated, offers)
	sendRevocations(revocations)
	return nil
}

func (h *hub) expireStreamRequest(roomID, requestID string) {
	h.mu.Lock()
	r := h.rooms[roomID]
	if r == nil || r.streamRequests[requestID] == nil || time.Now().Before(r.streamRequests[requestID].expiresAt) {
		h.mu.Unlock()
		return
	}
	request := r.streamRequests[requestID]
	delete(r.streamRequests, requestID)
	revocations := revocationsFor(request.provider, request.viewer, request.id, request.offerID, "stream request expired")
	h.mu.Unlock()
	sendRevocations(revocations)
}

func (h *hub) expireStreamGrant(roomID, requestID string) {
	h.mu.Lock()
	r := h.rooms[roomID]
	if r == nil || r.streamGrants[requestID] == nil || time.Now().Before(r.streamGrants[requestID].expiresAt) {
		h.mu.Unlock()
		return
	}
	grant := r.streamGrants[requestID]
	delete(r.streamGrants, requestID)
	decrementOfferViewers(r, grant.offerID)
	revocations := revocationsFor(grant.provider, grant.viewer, grant.requestID, grant.offerID, "stream grant expired")
	recipients, offers := streamSessionsOf(r), streamOffersOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeStreamOffersUpdated, offers)
	sendRevocations(revocations)
}

func validateStreamMedia(media protocol.Media) error {
	media.Title = strings.TrimSpace(media.Title)
	if media.Title == "" || utf8.RuneCountInString(media.Title) > maxMediaTitleRunes || media.SizeBytes <= 0 || media.DurationSeconds < 0 || math.IsNaN(media.DurationSeconds) || math.IsInf(media.DurationSeconds, 0) {
		return invalid("invalid_stream_offer", "stream offer media metadata is invalid")
	}
	const prefix = "file-v1:"
	if !strings.HasPrefix(media.Fingerprint, prefix) || len(media.Fingerprint) != len(prefix)+sha256.Size*2 {
		return invalid("invalid_stream_offer", "only a valid file-v1 media identity can be offered")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(media.Fingerprint, prefix)); err != nil {
		return invalid("invalid_stream_offer", "only file-v1 media can be offered")
	}
	return nil
}

func removeParticipantStreamsLocked(r *room, p *participant, reason string) []directedRevocation {
	var result []directedRevocation
	for offerID, offer := range r.streamOffers {
		if offer.ProviderID == p.state.ID {
			result = append(result, removeOfferStreamsLocked(r, offerID, reason)...)
			delete(r.streamOffers, offerID)
		}
	}
	for id, request := range r.streamRequests {
		if request.provider == p || request.viewer == p {
			delete(r.streamRequests, id)
			result = append(result, revocationsFor(request.provider, request.viewer, id, request.offerID, reason)...)
		}
	}
	for id, grant := range r.streamGrants {
		if grant.provider == p || grant.viewer == p {
			delete(r.streamGrants, id)
			decrementOfferViewers(r, grant.offerID)
			result = append(result, revocationsFor(grant.provider, grant.viewer, id, grant.offerID, reason)...)
		}
	}
	return result
}

func removeOfferStreamsLocked(r *room, offerID, reason string) []directedRevocation {
	var result []directedRevocation
	for id, request := range r.streamRequests {
		if request.offerID == offerID {
			delete(r.streamRequests, id)
			result = append(result, revocationsFor(request.provider, request.viewer, id, offerID, reason)...)
		}
	}
	for id, grant := range r.streamGrants {
		if grant.offerID == offerID {
			delete(r.streamGrants, id)
			result = append(result, revocationsFor(grant.provider, grant.viewer, id, offerID, reason)...)
		}
	}
	return result
}

func revocationsFor(provider, viewer *participant, requestID, offerID, reason string) []directedRevocation {
	payload := protocol.MediaStreamRevoked{RequestID: requestID, OfferID: offerID, Reason: reason}
	result := make([]directedRevocation, 0, 2)
	if provider != nil && provider.session.supports(protocol.CapabilityMediaStreamV1) {
		result = append(result, directedRevocation{recipient: provider.session, payload: payload})
	}
	if viewer != nil && viewer != provider && viewer.session.supports(protocol.CapabilityMediaStreamV1) {
		result = append(result, directedRevocation{recipient: viewer.session, payload: payload})
	}
	return result
}

func sendRevocations(values []directedRevocation) {
	for _, value := range values {
		value.recipient.sendMessage(protocol.TypeStreamRevoked, "", "", value.payload)
	}
}

func decrementOfferViewers(r *room, offerID string) {
	if offer := r.streamOffers[offerID]; offer != nil && offer.ViewerCount > 0 {
		offer.ViewerCount--
	}
}

func offerHasTransfersLocked(r *room, offerID string) bool {
	for _, request := range r.streamRequests {
		if request.offerID == offerID {
			return true
		}
	}
	for _, grant := range r.streamGrants {
		if grant.offerID == offerID {
			return true
		}
	}
	return false
}

func (h *hub) finishPlaylistWheel(roomID, wheelID string) {
	h.mu.Lock()
	r := h.rooms[roomID]
	if r == nil || r.wheel == nil || r.wheel.ID != wheelID {
		h.mu.Unlock()
		return
	}
	if r.wheelTimer != nil {
		r.wheelTimer.Stop()
		r.wheelTimer = nil
	}
	wheel := cloneWheel(r.wheel)
	if wheel.Winner < 0 || wheel.Winner >= len(wheel.Items) || r.playlist.Revision != wheel.PlaylistRevision {
		cancelled := cancelWheelLocked(r, "The queue changed before the spin finished")
		recipients := sessionsOf(r)
		h.mu.Unlock()
		broadcast(recipients, protocol.TypePlaylistWheelUpdated, *cancelled)
		return
	}
	winnerID := wheel.Items[wheel.Winner].ID
	selected := -1
	for index := range r.playlist.Items {
		if r.playlist.Items[index].ID == winnerID {
			selected = index
			break
		}
	}
	if selected < 0 {
		cancelled := cancelWheelLocked(r, "The winning item is no longer in the queue")
		recipients := sessionsOf(r)
		h.mu.Unlock()
		broadcast(recipients, protocol.TypePlaylistWheelUpdated, *cancelled)
		return
	}
	r.wheel = nil
	wheel.Phase = protocol.PlaylistWheelCompleted
	r.playlist.Revision++
	r.playlist.Selected = selected
	r.playback.Revision++
	r.playback.PositionSeconds = 0
	r.playback.Paused = false
	r.playback.Rate = 1
	r.playback.UpdatedAtUnixMs = time.Now().UnixMilli()
	r.playback.SetBy = "wheel:" + wheel.ID
	playback := r.playback
	r.playback.Seek = true
	playback.Seek = true
	recipients := sessionsOf(r)
	playlist := clonePlaylist(r.playlist)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypePlaylistWheelUpdated, *wheel)
	broadcast(recipients, protocol.TypePlaylistUpdated, playlist)
	broadcast(recipients, protocol.TypePlaybackUpdated, playback)
	broadcast(recipients, protocol.TypeActivityMessage, protocol.ActivityMessage{
		Action: protocol.ActivityWheelCompleted, ParticipantID: wheel.RequesterID, ParticipantName: wheel.RequesterName,
		ItemLabel: playlist.Items[selected].Label, SentAtUnixMs: time.Now().UnixMilli(),
	})
}

func cancelWheelLocked(r *room, reason string) *protocol.PlaylistWheel {
	if r == nil || r.wheel == nil {
		return nil
	}
	wheel := cloneWheel(r.wheel)
	wheel.Phase = protocol.PlaylistWheelCancelled
	wheel.Reason = reason
	r.wheel = nil
	if r.wheelTimer != nil {
		r.wheelTimer.Stop()
		r.wheelTimer = nil
	}
	return wheel
}

func (h *hub) chat(p *participant, request protocol.ChatSend) error {
	message := strings.TrimSpace(request.Message)
	if message == "" || utf8.RuneCountInString(message) > maxChatRunes {
		return invalid("invalid_chat", "chat message is empty or too long")
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return invalid("not_joined", "the participant is not in a room")
	}
	recipients := sessionsOf(r)
	participantID, participantName := p.state.ID, p.state.Name
	h.mu.Unlock()
	id, err := randomToken()
	if err != nil {
		return err
	}
	broadcast(recipients, protocol.TypeChatMessage, protocol.ChatMessage{
		ID: id, ParticipantID: participantID, ParticipantName: participantName,
		Message: message, SentAtUnixMs: time.Now().UnixMilli(),
	})
	return nil
}

func activityMessage(participantID, participantName string, action protocol.ActivityAction, position, rate float64, itemLabel string, itemCount int) protocol.ActivityMessage {
	return protocol.ActivityMessage{
		Action: action, PositionSeconds: position, Rate: rate, ItemLabel: itemLabel,
		ItemCount: itemCount, SentAtUnixMs: time.Now().UnixMilli(),
		ParticipantID: participantID, ParticipantName: participantName,
	}
}

func (h *hub) roomForLocked(p *participant) *room {
	if p == nil {
		return nil
	}
	for _, r := range h.rooms {
		if r.participants[p.state.ID] == p {
			return r
		}
	}
	return nil
}

func sessionsOf(r *room) []*session {
	result := make([]*session, 0, len(r.participants))
	for _, current := range r.participants {
		result = append(result, current.session)
	}
	return result
}

func streamSessionsOf(r *room) []*session {
	result := make([]*session, 0, len(r.participants))
	for _, current := range r.participants {
		if current.session.supports(protocol.CapabilityMediaStreamV1) {
			result = append(result, current.session)
		}
	}
	return result
}

func streamOffersOf(r *room) []protocol.MediaStreamOffer {
	result := make([]protocol.MediaStreamOffer, 0, len(r.streamOffers))
	for _, offer := range r.streamOffers {
		result = append(result, *offer)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ProviderName != result[j].ProviderName {
			return result[i].ProviderName < result[j].ProviderName
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func participantsOf(r *room) []protocol.Participant {
	result := make([]protocol.Participant, 0, len(r.participants))
	for _, current := range r.participants {
		state := current.state
		state.Media = cloneMedia(state.Media)
		state.AvailableMedia = append([]string(nil), state.AvailableMedia...)
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func broadcast(recipients []*session, messageType protocol.MessageType, payload any) {
	for _, recipient := range recipients {
		recipient.sendMessage(messageType, "", "", payload)
	}
}

func validateText(field, value string, maximum int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", invalid("invalid_"+field, field+" is required")
	}
	if strings.ContainsAny(value, "\r\n\x00") || utf8.RuneCountInString(value) > maximum {
		return "", invalid("invalid_"+field, fmt.Sprintf("%s must not exceed %d characters", field, maximum))
	}
	return value, nil
}

func validatePlaylist(items []protocol.PlaylistItem) ([]protocol.PlaylistItem, error) {
	if len(items) > maxPlaylistItems {
		return nil, invalid("invalid_playlist", "playlist has too many items")
	}
	result := make([]protocol.PlaylistItem, len(items))
	total := 0
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		item.Label = strings.TrimSpace(item.Label)
		item.URL = strings.TrimSpace(item.URL)
		if item.Label == "" || (item.URL == "" && (item.Media == nil || item.Media.Fingerprint == "")) {
			return nil, invalid("invalid_playlist", "playlist items require a label and a URL or media identity")
		}
		if item.URL != "" {
			parsed, err := url.Parse(item.URL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return nil, invalid("invalid_playlist", "playlist URLs must use HTTP or HTTPS")
			}
		}
		if item.Media != nil {
			item.Media = cloneMedia(item.Media)
			item.Media.Title = strings.TrimSpace(item.Media.Title)
			if item.Media.Title == "" || len(item.Media.Fingerprint) > 128 || item.Media.SizeBytes < 0 || item.Media.DurationSeconds < 0 || math.IsNaN(item.Media.DurationSeconds) || math.IsInf(item.Media.DurationSeconds, 0) {
				return nil, invalid("invalid_playlist", "playlist media identity is invalid")
			}
			total += len(item.Media.Title) + len(item.Media.Fingerprint)
		}
		total += len(item.Label) + len(item.URL)
		if total > maxPlaylistText {
			return nil, invalid("invalid_playlist", "playlist text is too large")
		}
		if item.ID == "" {
			generated, err := randomToken()
			if err != nil {
				return nil, err
			}
			item.ID = generated
		}
		if len(item.ID) > 128 {
			return nil, invalid("invalid_playlist", "playlist item ID is too long")
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, invalid("invalid_playlist", "playlist item IDs must be unique")
		}
		seen[item.ID] = struct{}{}
		result[index] = item
	}
	return result, nil
}

func freeName(base string, participants map[string]*participant) string {
	used := make(map[string]struct{}, len(participants))
	for _, current := range participants {
		used[strings.ToLower(current.state.Name)] = struct{}{}
	}
	if _, exists := used[strings.ToLower(base)]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s (%d)", base, suffix)
		if _, exists := used[strings.ToLower(candidate)]; !exists {
			return candidate
		}
	}
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func secureRandomInt(maximum int) (int, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func secureRandomUint64() (uint64, error) {
	// A 32-bit visual seed is ample for animation variety and remains exactly
	// representable by every JSON/JavaScript client.
	var data [4]byte
	if _, err := rand.Read(data[:]); err != nil {
		return 0, err
	}
	return uint64(binary.BigEndian.Uint32(data[:])), nil
}

func cloneMedia(media *protocol.Media) *protocol.Media {
	if media == nil {
		return nil
	}
	copy := *media
	return &copy
}

func clonePlaylist(playlist protocol.Playlist) protocol.Playlist {
	items := make([]protocol.PlaylistItem, len(playlist.Items))
	copy(items, playlist.Items)
	playlist.Items = items
	for index := range playlist.Items {
		playlist.Items[index].Media = cloneMedia(playlist.Items[index].Media)
	}
	return playlist
}

func cloneWheel(wheel *protocol.PlaylistWheel) *protocol.PlaylistWheel {
	if wheel == nil {
		return nil
	}
	value := *wheel
	value.Items = append([]protocol.PlaylistWheelItem(nil), wheel.Items...)
	return &value
}

func asCommandError(err error) *commandError {
	var target *commandError
	if errors.As(err, &target) {
		return target
	}
	return &commandError{code: "internal", message: "the server could not complete the command"}
}

func (h *hub) setMediaAvailability(p *participant, request protocol.MediaAvailabilitySet) error {
	if len(request.Fingerprints) > maxPlaylistItems {
		return invalid("invalid_availability", "too many media fingerprints")
	}
	seen := make(map[string]bool)
	for _, fp := range request.Fingerprints {
		if fp == "" || len(fp) > 128 || seen[fp] {
			return invalid("invalid_availability", "invalid or duplicate media fingerprint")
		}
		seen[fp] = true
	}
	h.mu.Lock()
	r := h.roomForLocked(p)
	if r == nil {
		h.mu.Unlock()
		return invalid("not_joined", "the participant is not in a room")
	}
	p.state.AvailableMedia = append([]string(nil), request.Fingerprints...)
	recipients, participants := sessionsOf(r), participantsOf(r)
	h.mu.Unlock()
	broadcast(recipients, protocol.TypeMediaUpdated, participants)
	return nil
}
