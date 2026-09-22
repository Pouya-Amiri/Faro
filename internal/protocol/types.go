package protocol

import (
	"encoding/json"
	"time"
)

const Version = 2

type MessageType string

const (
	TypeHello                 MessageType = "hello"
	TypeWelcome               MessageType = "welcome"
	TypeError                 MessageType = "error"
	TypeCommandOK             MessageType = "command.ok"
	TypePing                  MessageType = "ping"
	TypePong                  MessageType = "pong"
	TypeStateSnapshot         MessageType = "state.snapshot"
	TypeRoomModeSet           MessageType = "room.mode.set"
	TypeRoomUpdated           MessageType = "room.updated"
	TypeRoomRoleSet           MessageType = "room.role.set"
	TypeParticipantsUpdated   MessageType = "participants.updated"
	TypePlaybackSet           MessageType = "playback.set"
	TypePlaybackUpdated       MessageType = "playback.updated"
	TypeMediaSet              MessageType = "media.set"
	TypeMediaUpdated          MessageType = "media.updated"
	TypeReadinessSet          MessageType = "readiness.set"
	TypeReadinessUpdated      MessageType = "readiness.updated"
	TypePlaylistSet           MessageType = "playlist.set"
	TypePlaylistSelect        MessageType = "playlist.select"
	TypePlaylistUpdated       MessageType = "playlist.updated"
	TypePlaylistWheelSpin     MessageType = "playlist.wheel.spin"
	TypePlaylistWheelUpdated  MessageType = "playlist.wheel.updated"
	TypeChatSend              MessageType = "chat.send"
	TypeChatMessage           MessageType = "chat.message"
	TypeActivityMessage       MessageType = "activity.message"
	TypeStreamOfferPublish    MessageType = "media.stream.offer.publish"
	TypeStreamOfferWithdraw   MessageType = "media.stream.offer.withdraw"
	TypeStreamOffersUpdated   MessageType = "media.stream.offers.updated"
	TypeStreamRequest         MessageType = "media.stream.request"
	TypeStreamRequestAccepted MessageType = "media.stream.request.accepted"
	TypeStreamRequested       MessageType = "media.stream.requested"
	TypeStreamGrant           MessageType = "media.stream.grant"
	TypeStreamGranted         MessageType = "media.stream.granted"
	TypeStreamRevoke          MessageType = "media.stream.revoke"
	TypeStreamRevoked         MessageType = "media.stream.revoked"
)

type Capability string

const CapabilityMediaStreamV1 Capability = "media-stream-v1"

type CommandOK struct {
	Revision uint64 `json:"revision,omitempty"`
}

type Envelope struct {
	Version int             `json:"version"`
	Type    MessageType     `json:"type"`
	ID      string          `json:"id,omitempty"`
	ReplyTo string          `json:"replyTo,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func NewEnvelope(messageType MessageType, id, replyTo string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Version: Version, Type: messageType, ID: id, ReplyTo: replyTo, Payload: raw}, nil
}

func DecodePayload[T any](envelope Envelope) (T, error) {
	var value T
	err := json.Unmarshal(envelope.Payload, &value)
	return value, err
}

type Role string

const (
	RoleOwner     Role = "owner"
	RoleModerator Role = "moderator"
	RoleMember    Role = "member"
)

type RoomMode string

const (
	RoomCollaborative RoomMode = "collaborative"
	RoomModerated     RoomMode = "moderated"
)

type Hello struct {
	ClientVersion string       `json:"clientVersion"`
	Name          string       `json:"name"`
	Room          string       `json:"room"`
	JoinToken     string       `json:"joinToken,omitempty"`
	OwnerToken    string       `json:"ownerToken,omitempty"`
	Capabilities  []Capability `json:"capabilities,omitempty"`
}

type Welcome struct {
	ServerVersion string             `json:"serverVersion"`
	SessionID     string             `json:"sessionId"`
	ParticipantID string             `json:"participantId"`
	RoomID        string             `json:"roomId"`
	OwnerToken    string             `json:"ownerToken,omitempty"`
	Capabilities  []Capability       `json:"capabilities,omitempty"`
	Streaming     *MediaStreamPolicy `json:"streaming,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

type Ping struct {
	ClientTimeUnixMs int64 `json:"clientTimeUnixMs"`
}

type Pong struct {
	ClientTimeUnixMs int64 `json:"clientTimeUnixMs"`
	ServerTimeUnixMs int64 `json:"serverTimeUnixMs"`
}

type Media struct {
	Title           string  `json:"title"`
	DurationSeconds float64 `json:"durationSeconds"`
	SizeBytes       int64   `json:"sizeBytes,omitempty"`
	Fingerprint     string  `json:"fingerprint,omitempty"`
}

type Participant struct {
	AvailableMedia []string `json:"availableMedia,omitempty"`
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Role           Role     `json:"role"`
	Ready          bool     `json:"ready"`
	Media          *Media   `json:"media,omitempty"`
}

type Playback struct {
	Revision        uint64  `json:"revision"`
	PositionSeconds float64 `json:"positionSeconds"`
	Paused          bool    `json:"paused"`
	Rate            float64 `json:"rate"`
	UpdatedAtUnixMs int64   `json:"updatedAtUnixMs"`
	SetBy           string  `json:"setBy,omitempty"`
	Seek            bool    `json:"seek,omitempty"`
}

func (p Playback) PositionAt(now time.Time) float64 {
	if p.Paused {
		return p.PositionSeconds
	}
	elapsed := float64(now.UnixMilli()-p.UpdatedAtUnixMs) / 1000
	if elapsed < 0 {
		elapsed = 0
	}
	return p.PositionSeconds + elapsed*p.Rate
}

type PlaylistItem struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
	Media *Media `json:"media,omitempty"`
}

type Playlist struct {
	Revision uint64         `json:"revision"`
	Items    []PlaylistItem `json:"items"`
	Selected int            `json:"selected"`
}

type PlaylistWheelPhase string

const (
	PlaylistWheelStarted   PlaylistWheelPhase = "started"
	PlaylistWheelCompleted PlaylistWheelPhase = "completed"
	PlaylistWheelCancelled PlaylistWheelPhase = "cancelled"
)

type PlaylistWheelItem struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// PlaylistWheel is a server-authored animation recipe. Clients render the
// same queue snapshot, winner, and trajectory without deciding the result.
type PlaylistWheel struct {
	ID               string              `json:"id"`
	Phase            PlaylistWheelPhase  `json:"phase"`
	RequesterID      string              `json:"requesterId"`
	RequesterName    string              `json:"requesterName"`
	PlaylistRevision uint64              `json:"playlistRevision"`
	Items            []PlaylistWheelItem `json:"items"`
	Winner           int                 `json:"winner"`
	Turns            int                 `json:"turns"`
	VisualSeed       uint64              `json:"visualSeed"`
	StartsAtUnixMs   int64               `json:"startsAtUnixMs"`
	DurationMs       int                 `json:"durationMs"`
	Reason           string              `json:"reason,omitempty"`
}

type Room struct {
	ID   string   `json:"id"`
	Mode RoomMode `json:"mode"`
}

type Snapshot struct {
	SelfID        string             `json:"selfId"`
	Room          Room               `json:"room"`
	Participants  []Participant      `json:"participants"`
	Playback      Playback           `json:"playback"`
	Playlist      Playlist           `json:"playlist"`
	PlaylistWheel *PlaylistWheel     `json:"playlistWheel,omitempty"`
	StreamOffers  []MediaStreamOffer `json:"streamOffers,omitempty"`
}

// MediaStreamPolicy is advertised only when the server has explicitly enabled
// the media-stream-v1 extension. The DERP map URL is operator policy; Tailcat
// types and connection state remain confined to the transport adapter.
type MediaStreamPolicy struct {
	MaxOffersPerParticipant int    `json:"maxOffersPerParticipant"`
	MaxViewersPerOffer      int    `json:"maxViewersPerOffer"`
	GrantTTLSeconds         int    `json:"grantTtlSeconds"`
	DERPMapURL              string `json:"derpMapUrl,omitempty"`
}

// MediaStreamOffer contains only room-visible metadata. A local path,
// connection descriptor, or bearer capability must never be added here.
type MediaStreamOffer struct {
	ID           string `json:"id"`
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	Media        Media  `json:"media"`
	Revision     uint64 `json:"revision"`
	MaxViewers   int    `json:"maxViewers"`
	ViewerCount  int    `json:"viewerCount"`
}

type MediaStreamOfferPublish struct {
	OfferID    string `json:"offerId"`
	Media      Media  `json:"media"`
	MaxViewers int    `json:"maxViewers,omitempty"`
}

type MediaStreamOfferWithdraw struct {
	OfferID string `json:"offerId"`
}

type MediaStreamRequest struct {
	OfferID         string `json:"offerId"`
	ClientPublicKey string `json:"clientPublicKey"`
}

type MediaStreamRequestAccepted struct {
	RequestID       string `json:"requestId"`
	OfferID         string `json:"offerId"`
	ExpiresAtUnixMs int64  `json:"expiresAtUnixMs"`
}

// MediaStreamRequested is directed only to the provider that owns the offer.
type MediaStreamRequested struct {
	RequestID       string `json:"requestId"`
	OfferID         string `json:"offerId"`
	ViewerID        string `json:"viewerId"`
	ViewerName      string `json:"viewerName"`
	ClientPublicKey string `json:"clientPublicKey"`
	ExpiresAtUnixMs int64  `json:"expiresAtUnixMs"`
}

// MediaStreamGrant is provider-authored secret material. The server routes it
// to one validated viewer and never stores it in room state.
type MediaStreamGrant struct {
	RequestID          string `json:"requestId"`
	ConnectionBlob     string `json:"connectionBlob"`
	TransferCapability string `json:"transferCapability"`
	ExpiresAtUnixMs    int64  `json:"expiresAtUnixMs"`
}

// MediaStreamGranted is directed only to the viewer that made the request.
type MediaStreamGranted struct {
	RequestID          string `json:"requestId"`
	OfferID            string `json:"offerId"`
	ProviderID         string `json:"providerId"`
	ProviderName       string `json:"providerName"`
	ConnectionBlob     string `json:"connectionBlob"`
	TransferCapability string `json:"transferCapability"`
	ExpiresAtUnixMs    int64  `json:"expiresAtUnixMs"`
}

type MediaStreamRevoke struct {
	RequestID string `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}

type MediaStreamRevoked struct {
	RequestID string `json:"requestId"`
	OfferID   string `json:"offerId"`
	Reason    string `json:"reason"`
}

type RoomModeSet struct {
	Mode RoomMode `json:"mode"`
}

type RoomRoleSet struct {
	ParticipantID string `json:"participantId"`
	Role          Role   `json:"role"`
}

type PlaybackSet struct {
	PositionSeconds float64 `json:"positionSeconds"`
	Paused          bool    `json:"paused"`
	Rate            float64 `json:"rate"`
	Seek            bool    `json:"seek,omitempty"`
	SponsorBlock    bool    `json:"sponsorBlock,omitempty"`
}

const TypeMediaAvailabilitySet MessageType = "media.availability.set"
const CapabilityMediaAvailabilityV1 Capability = "media-availability-v1"

type MediaAvailabilitySet struct {
	Fingerprints []string `json:"fingerprints"`
}

type MediaSet struct {
	Media *Media `json:"media"`
}

type ReadinessSet struct {
	Ready bool `json:"ready"`
}

type PlaylistSet struct {
	Items []PlaylistItem `json:"items"`
}

type PlaylistSelect struct {
	Index int `json:"index"`
}

type PlaylistWheelSpin struct{}

type ChatSend struct {
	Message string `json:"message"`
}

type ChatMessage struct {
	ID              string `json:"id"`
	ParticipantID   string `json:"participantId"`
	ParticipantName string `json:"participantName"`
	Message         string `json:"message"`
	SentAtUnixMs    int64  `json:"sentAtUnixMs"`
}

type ActivityAction string

const (
	ActivityPlaybackPaused  ActivityAction = "playback.paused"
	ActivityPlaybackResumed ActivityAction = "playback.resumed"
	ActivityPlaybackSeeked  ActivityAction = "playback.seeked"
	ActivityPlaybackRate    ActivityAction = "playback.rate"
	ActivitySponsorSkipped  ActivityAction = "sponsorblock.skipped"
	ActivityPlaylistUpdated ActivityAction = "playlist.updated"
	ActivityPlaylistPlayed  ActivityAction = "playlist.played"
	ActivityWheelStarted    ActivityAction = "wheel.started"
	ActivityWheelCompleted  ActivityAction = "wheel.completed"
)

// ActivityMessage is a server-authored, ephemeral room event. Clients format
// the typed action for presentation instead of trusting arbitrary event text.
type ActivityMessage struct {
	Action          ActivityAction `json:"action"`
	ParticipantID   string         `json:"participantId,omitempty"`
	ParticipantName string         `json:"participantName,omitempty"`
	ItemLabel       string         `json:"itemLabel,omitempty"`
	ItemCount       int            `json:"itemCount,omitempty"`
	PositionSeconds float64        `json:"positionSeconds,omitempty"`
	DeltaSeconds    float64        `json:"deltaSeconds,omitempty"`
	Rate            float64        `json:"rate,omitempty"`
	SentAtUnixMs    int64          `json:"sentAtUnixMs"`
}
