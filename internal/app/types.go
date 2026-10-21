package app

import (
	"context"
	"sync"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/mediastream"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	faroserver "github.com/Pouya-Amiri/Faro/internal/server"
	"github.com/Pouya-Amiri/Faro/internal/streamtransport"
	tailcattransport "github.com/Pouya-Amiri/Faro/internal/streamtransport/tailcat"
	"github.com/Pouya-Amiri/Faro/internal/syncer"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

type youtubeResolver interface {
	Inspect(context.Context, string) (youtube.Info, error)
	Resolve(context.Context, string, int) (youtube.Stream, error)
}

type ConnectionRequest struct {
	Invite        string   `json:"invite"`
	Name          string   `json:"name"`
	Player        string   `json:"player"`
	Executable    string   `json:"executable,omitempty"`
	PlayerArgs    []string `json:"playerArgs,omitempty"`
	ClientVersion string   `json:"clientVersion"`
	// Preferences, when given, are applied before anything in the room can
	// start automatically, so the first stream already uses the saved cache
	// policy and the first file is shared only if sharing is enabled.
	Preferences *Preferences `json:"preferences,omitempty"`
}

// Preferences are this device's settings for automatic behavior in a room.
type Preferences struct {
	SponsorBlock bool `json:"sponsorBlock"`
	AutoOffer    bool `json:"autoOffer"`
	ChatOverlay  bool `json:"chatOverlay"`
	// StreamCacheLimit is passed to SetStreamCacheLimit.
	StreamCacheLimit int64 `json:"streamCacheLimit"`
}

type Event struct {
	Kind            string                    `json:"kind"`
	Snapshot        *protocol.Snapshot        `json:"snapshot,omitempty"`
	Chat            *protocol.ChatMessage     `json:"chat,omitempty"`
	Activity        *protocol.ActivityMessage `json:"activity,omitempty"`
	Error           *protocol.Error           `json:"error,omitempty"`
	Connection      *ConnectionStatus         `json:"connection,omitempty"`
	Wheel           *protocol.PlaylistWheel   `json:"wheel,omitempty"`
	Stream          *StreamStatus             `json:"stream,omitempty"`
	StreamChat      *StreamChat               `json:"streamChat,omitempty"`
	Sync            *SyncStatus               `json:"sync,omitempty"`
	ServerNowUnixMs int64                     `json:"serverNowUnixMs,omitempty"`
}

// StreamStatus is desktop-local lifecycle state. It is deliberately not part
// of the room snapshot or wire protocol: only the viewer needs to know whether
// its loopback gateway is being prepared or is active.
type StreamStatus struct {
	// State is "requesting", "active" or "idle".
	State   string `json:"state"`
	OfferID string `json:"offerId,omitempty"`
	// Route describes an active stream's connection to its provider:
	// "direct", "unavailable" (no direct path right now), "reconnecting"
	// (the grant ended and a new one is being requested) or "cached" (the
	// whole file is on this device and no provider is needed).
	Route string `json:"route,omitempty"`
	// Error is the latest transfer problem of an active stream.
	Error string `json:"error,omitempty"`
	// Cache progress of an active stream, reported about once a second.
	CachedBytes    int64   `json:"cachedBytes,omitempty"`
	TotalBytes     int64   `json:"totalBytes,omitempty"`
	BytesPerSecond float64 `json:"bytesPerSecond,omitempty"`
	// CachedRanges are the cached parts as [start, end) fractions of the file.
	CachedRanges [][2]float64 `json:"cachedRanges,omitempty"`
}

// StreamChat carries the live chat of the broadcast this participant's
// player shows, kept apart from the room's own chat. It is read only.
type StreamChat struct {
	// Session increases with every broadcast whose chat is followed, so
	// updates from a chat that was already left can be ignored.
	Session uint64 `json:"session"`
	// State is "active" while messages arrive, then "ended" or
	// "unavailable" (Message says why).
	State    string                `json:"state"`
	Message  string                `json:"message,omitempty"`
	Messages []youtube.ChatMessage `json:"messages,omitempty"`
	Removed  []string              `json:"removed,omitempty"`
}

// SyncStatus is how closely this participant's own player follows the room.
// Other participants' positions are not shared, so it cannot speak for them.
type SyncStatus struct {
	// State is "synced", "catching-up", "buffering", "live" (a live
	// broadcast: only playing and pausing are followed) or "idle" (no
	// player, or media still loading).
	State        string  `json:"state"`
	DriftSeconds float64 `json:"driftSeconds,omitempty"`
}

type ConnectionStatus struct {
	State   string `json:"state"`
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
}

type PlaylistInput struct {
	ID     string          `json:"id,omitempty"`
	Label  string          `json:"label"`
	Source string          `json:"source,omitempty"`
	URL    string          `json:"url,omitempty"`
	Media  *protocol.Media `json:"media,omitempty"`
	// DurationSeconds is a duration the page already knows, such as the one
	// yt-dlp reported for a YouTube link.
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
}

type ServerRequest struct {
	Mode                string `json:"mode,omitempty"`
	ListenAddress       string `json:"listenAddress"`
	PublicHost          string `json:"publicHost"`
	Room                string `json:"room"`
	Protected           bool   `json:"protected"`
	DisableStreaming    bool   `json:"disableStreaming,omitempty"`
	StreamingDERPMapURL string `json:"streamingDerpMapUrl,omitempty"`
}

type LocalServerStatus struct {
	Running       bool   `json:"running"`
	ListenAddress string `json:"listenAddress,omitempty"`
	ShareInvite   string `json:"shareInvite,omitempty"`
	LocalInvite   string `json:"localInvite,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
}

type TimelineSegment struct {
	StartSeconds float64 `json:"startSeconds"`
	EndSeconds   float64 `json:"endSeconds,omitempty"`
	Title        string  `json:"title"`
	Category     string  `json:"category,omitempty"`
	Kind         string  `json:"kind"`
}

type EventSink func(Event)

type Service struct {
	root context.Context
	sink EventSink

	playerLifecycleMu  sync.Mutex
	streamOfferMu      sync.Mutex
	mediaTransitionMu  sync.Mutex
	mediaLoadMu        sync.Mutex
	selectionMu        sync.Mutex
	mediaPublishMu     sync.Mutex
	availabilityMu     sync.Mutex
	availabilityClient *faroclient.Client
	availabilitySent   []string
	syncApplyMu        sync.Mutex
	log                func(format string, args ...any)
	// identity is this installation's secret, from which rooms learn who to
	// keep out after a ban. Set once before the service is used.
	identity string

	mu                      sync.RWMutex
	sessionCtx              context.Context
	connectCancel           context.CancelFunc
	connectAttempt          uint64
	cancel                  context.CancelFunc
	client                  *faroclient.Client
	player                  player.Player
	playerCancel            context.CancelFunc
	playerContext           context.Context
	playerDismissed         bool
	playerCloseGeneration   uint64
	wheelID                 string
	wheelCloseGeneration    uint64
	sync                    *syncer.Controller
	invite                  invite.Invite
	ownerToken              string
	lastPlayer              player.State
	expectedPause           *bool
	expectedSeek            *float64
	expectedRate            *float64
	expectedUntil           time.Time
	suppressUntil           time.Time
	localPlaybackPending    bool
	openingMedia            bool
	transitionPaused        *bool
	selectedItem            string
	selectedItemIdentity    string
	load                    *mediaLoad
	mediaGen                uint64
	published               publishedMedia
	currentLive             bool
	liveProbe               liveProbe
	recovery                playbackRecovery
	liveChat                *liveChatReader
	liveChatSession         uint64
	lastRemoteRevision      uint64
	currentSource           string
	manualSource            string
	currentPlayerSource     string
	youtubeQualities        map[string]int
	sponsorBlock            bool
	sponsorSegments         []youtube.Segment
	lastSponsorEnd          float64
	request                 ConnectionRequest
	startPlayer             func(context.Context, ConnectionRequest) (player.Player, error)
	youtube                 youtubeResolver
	sources                 map[string]localCopy
	localServer             *faroserver.Server
	serverStarting          bool
	serverCancel            context.CancelFunc
	serverStatus            LocalServerStatus
	streamFactory           streamtransport.Factory
	streamPublisher         *mediastream.HostedPublisher
	streamOfferID           string
	streamOfferItemID       string
	streamOfferMedia        *protocol.Media
	streamOfferAuto         bool
	autoOffer               bool
	autoOfferTimer          *time.Timer
	autoOfferKey            string
	autoOfferSuppressedItem string
	offerStart              *cancelHandle
	streamCapabilities      map[string]string
	stream                  *viewerStream
	streamSeq               uint64
	streamFailures          map[failureKey]*streamFailure
	streamSuppressedItem    string
	streamRetry             *time.Timer
	chat                    chatOverlay
	streamCache             mediastream.CacheOptions
	syncState               string
	syncDrift               float64
}

func New(root context.Context, sink EventSink) *Service {
	if root == nil {
		root = context.Background()
	}
	if sink == nil {
		sink = func(Event) {}
	}
	return &Service{
		root: root, sink: sink, sources: make(map[string]localCopy), startPlayer: startPlayer, log: func(string, ...any) {},
		youtube: youtube.NewResolver(), youtubeQualities: make(map[string]int), sponsorBlock: true, autoOffer: true,
		streamFactory: tailcattransport.Factory{}, streamCapabilities: make(map[string]string),
		streamFailures: make(map[failureKey]*streamFailure),
	}
}

// SetDiagnostics directs diagnostic messages, for example about stream
// transfers, to logf. It must be called before the service is used.
func (s *Service) SetDiagnostics(logf func(format string, args ...any)) {
	if logf != nil {
		s.log = logf
	}
}

// SetIdentity sets this installation's secret identity. It must be called
// before the service is used; without it, this participant cannot be banned
// and rooms refuse to ban it.
func (s *Service) SetIdentity(identity string) {
	s.identity = identity
}
