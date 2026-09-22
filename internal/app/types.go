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
	ServerNowUnixMs int64                     `json:"serverNowUnixMs,omitempty"`
}

// StreamStatus is desktop-local lifecycle state. It is deliberately not part
// of the room snapshot or wire protocol: only the viewer needs to know whether
// its loopback gateway is being prepared or is active.
type StreamStatus struct {
	State   string `json:"state"`
	OfferID string `json:"offerId,omitempty"`
	Route   string `json:"route,omitempty"`
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
	mediaTransitionMu  sync.Mutex
	mediaLoadMu        sync.Mutex
	availabilityMu     sync.Mutex
	availabilityClient *faroclient.Client
	availabilitySent   []string
	syncApplyMu        sync.Mutex

	mu                     sync.RWMutex
	sessionCtx             context.Context
	cancel                 context.CancelFunc
	client                 *faroclient.Client
	player                 player.Player
	playerCancel           context.CancelFunc
	playerContext          context.Context
	playerDismissed        bool
	sync                   *syncer.Controller
	invite                 invite.Invite
	ownerToken             string
	lastPlayer             player.State
	expectedPause          *bool
	expectedSeek           *float64
	expectedRate           *float64
	expectedUntil          time.Time
	suppressUntil          time.Time
	localPlaybackPending   bool
	openingMedia           bool
	transitionPaused       *bool
	selectedItem           string
	selectedItemIdentity   string
	lastRemoteRevision     uint64
	currentSource          string
	currentPlayerSource    string
	youtubeQualities       map[string]int
	sponsorBlock           bool
	sponsorSegments        []youtube.Segment
	lastSponsorEnd         float64
	request                ConnectionRequest
	startPlayer            func(context.Context, ConnectionRequest) (player.Player, error)
	youtube                youtubeResolver
	sources                map[string]string
	localServer            *faroserver.Server
	serverStarting         bool
	serverCancel           context.CancelFunc
	serverStatus           LocalServerStatus
	streamFactory          streamtransport.Factory
	streamPublisher        *mediastream.HostedPublisher
	streamOfferID          string
	streamOfferItemID      string
	streamOfferMedia       *protocol.Media
	streamCapabilities     map[string]string
	pendingStreams         map[string]*pendingStream
	pendingRequestOffers   map[string]string
	streamGateway          *mediastream.Gateway
	streamActivationCancel context.CancelFunc
	streamActivation       *pendingStream
	streamRequestID        string
	streamReceiveItemID    string
	streamIdentity         *protocol.Media
}

func New(root context.Context, sink EventSink) *Service {
	if root == nil {
		root = context.Background()
	}
	if sink == nil {
		sink = func(Event) {}
	}
	return &Service{
		root: root, sink: sink, sources: make(map[string]string), startPlayer: startPlayer,
		youtube: youtube.NewResolver(), youtubeQualities: make(map[string]int), sponsorBlock: true,
		streamFactory: tailcattransport.Factory{}, streamCapabilities: make(map[string]string),
		pendingStreams: make(map[string]*pendingStream), pendingRequestOffers: make(map[string]string),
	}
}
