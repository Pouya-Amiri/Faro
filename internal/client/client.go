package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const (
	connectTimeout = 10 * time.Second
	pingInterval   = 5 * time.Second
	commandTimeout = 5 * time.Second
)

type commandReply struct {
	envelope protocol.Envelope
	err      error
}

type Event struct {
	Type          protocol.MessageType
	Error         *protocol.Error
	Chat          *protocol.ChatMessage
	Activity      *protocol.ActivityMessage
	OwnerToken    string
	Wheel         *protocol.PlaylistWheel
	StreamRequest *protocol.MediaStreamRequested
	StreamGrant   *protocol.MediaStreamGranted
	StreamRevoked *protocol.MediaStreamRevoked
}

type Client struct {
	cfg       Config
	conn      net.Conn
	codec     *protocol.Codec
	events    chan Event
	done      chan struct{}
	once      sync.Once
	seq       atomic.Uint64
	pendingMu sync.Mutex
	pending   map[string]chan commandReply

	mu       sync.RWMutex
	welcome  protocol.Welcome
	snapshot protocol.Snapshot
	clock    clockEstimator
}

func Connect(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsOptions, err := tlsConfig(cfg.ServerName, cfg.Fingerprint)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: connectTimeout}
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsOptions}
	var rawConnection net.Conn
	if cfg.DialContext == nil {
		rawConnection, err = tlsDialer.DialContext(ctx, "tcp", cfg.Address)
	} else {
		dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		var transport net.Conn
		transport, err = cfg.DialContext(dialCtx, "tcp", cfg.Address)
		if err == nil {
			secure := tls.Client(transport, tlsOptions)
			err = secure.HandshakeContext(dialCtx)
			if err != nil {
				_ = transport.Close()
			} else {
				rawConnection = secure
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("connect to Faro server: %w", err)
	}
	connection, ok := rawConnection.(*tls.Conn)
	if !ok {
		_ = rawConnection.Close()
		return nil, errors.New("TLS dialer returned a non-TLS connection")
	}
	client := &Client{
		cfg: cfg, conn: connection, codec: protocol.NewCodec(connection),
		events: make(chan Event, 128), done: make(chan struct{}),
		pending: make(map[string]chan commandReply),
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(connectTimeout))
	}
	helloID := client.nextID()
	if err := client.write(protocol.TypeHello, helloID, protocol.Hello{
		ClientVersion: cfg.ClientVersion, Name: cfg.Name, Room: cfg.Room,
		JoinToken: cfg.JoinToken, OwnerToken: cfg.OwnerToken, Capabilities: append([]protocol.Capability(nil), cfg.Capabilities...),
	}); err != nil {
		connection.Close()
		return nil, err
	}
	if err := client.readHandshake(helloID); err != nil {
		connection.Close()
		return nil, err
	}
	_ = connection.SetDeadline(time.Time{})
	go client.readLoop()
	go client.pingLoop()
	return client, nil
}

func (c *Client) readHandshake(helloID string) error {
	gotWelcome := false
	for !gotWelcome || c.snapshot.SelfID == "" {
		envelope, err := c.codec.Read()
		if err != nil {
			return err
		}
		switch envelope.Type {
		case protocol.TypeError:
			failure, decodeErr := protocol.DecodePayload[protocol.Error](envelope)
			if decodeErr != nil {
				return decodeErr
			}
			return fmt.Errorf("server rejected connection (%s): %s", failure.Code, failure.Message)
		case protocol.TypeWelcome:
			if envelope.ReplyTo != helloID {
				return errors.New("welcome did not answer hello")
			}
			welcome, decodeErr := protocol.DecodePayload[protocol.Welcome](envelope)
			if decodeErr != nil {
				return decodeErr
			}
			c.welcome = welcome
			gotWelcome = true
		case protocol.TypeStateSnapshot:
			snapshot, decodeErr := protocol.DecodePayload[protocol.Snapshot](envelope)
			if decodeErr != nil {
				return decodeErr
			}
			c.snapshot = snapshot
		}
	}
	return nil
}

func (c *Client) readLoop() {
	defer c.Close()
	for {
		envelope, err := c.codec.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				c.emit(Event{Type: protocol.TypeError, Error: &protocol.Error{Code: "connection_lost", Message: err.Error(), Fatal: true}})
			}
			return
		}
		c.apply(envelope)
	}
}

func (c *Client) apply(envelope protocol.Envelope) {
	if envelope.ReplyTo != "" && (envelope.Type == protocol.TypeCommandOK || envelope.Type == protocol.TypeError || envelope.Type == protocol.TypeRoomMoved || envelope.Type == protocol.TypeStreamRequestAccepted) {
		if c.deliverReply(envelope) && envelope.Type != protocol.TypeRoomMoved {
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	event := Event{Type: envelope.Type}
	switch envelope.Type {
	case protocol.TypeStateSnapshot:
		value, err := protocol.DecodePayload[protocol.Snapshot](envelope)
		if err != nil {
			return
		}
		c.snapshot = value
	case protocol.TypeRoomUpdated:
		value, err := protocol.DecodePayload[protocol.Room](envelope)
		if err != nil {
			return
		}
		c.snapshot.Room = value
	case protocol.TypeParticipantsUpdated, protocol.TypeMediaUpdated, protocol.TypeReadinessUpdated:
		value, err := protocol.DecodePayload[[]protocol.Participant](envelope)
		if err != nil {
			return
		}
		c.snapshot.Participants = value
	case protocol.TypePlaybackUpdated:
		value, err := protocol.DecodePayload[protocol.Playback](envelope)
		if err != nil {
			return
		}
		c.snapshot.Playback = value
	case protocol.TypePlaylistUpdated:
		value, err := protocol.DecodePayload[protocol.Playlist](envelope)
		if err != nil {
			return
		}
		c.snapshot.Playlist = value
	case protocol.TypePlaylistWheelUpdated:
		value, err := protocol.DecodePayload[protocol.PlaylistWheel](envelope)
		if err != nil {
			return
		}
		if value.Phase == protocol.PlaylistWheelStarted {
			c.snapshot.PlaylistWheel = cloneWheel(&value)
		} else {
			c.snapshot.PlaylistWheel = nil
		}
		event.Wheel = cloneWheel(&value)
	case protocol.TypeStreamOffersUpdated:
		value, err := protocol.DecodePayload[[]protocol.MediaStreamOffer](envelope)
		if err != nil {
			return
		}
		c.snapshot.StreamOffers = append([]protocol.MediaStreamOffer(nil), value...)
	case protocol.TypeStreamRequested:
		value, err := protocol.DecodePayload[protocol.MediaStreamRequested](envelope)
		if err != nil {
			return
		}
		event.StreamRequest = &value
	case protocol.TypeStreamGranted:
		value, err := protocol.DecodePayload[protocol.MediaStreamGranted](envelope)
		if err != nil {
			return
		}
		event.StreamGrant = &value
	case protocol.TypeStreamRevoked:
		value, err := protocol.DecodePayload[protocol.MediaStreamRevoked](envelope)
		if err != nil {
			return
		}
		event.StreamRevoked = &value
	case protocol.TypePong:
		value, err := protocol.DecodePayload[protocol.Pong](envelope)
		if err != nil {
			return
		}
		c.clock.observe(value.ClientTimeUnixMs, value.ServerTimeUnixMs, time.Now().UnixMilli())
	case protocol.TypeWelcome:
		value, err := protocol.DecodePayload[protocol.Welcome](envelope)
		if err != nil {
			return
		}
		c.welcome = value
		event.OwnerToken = value.OwnerToken
	case protocol.TypeRoomMoved:
		value, err := protocol.DecodePayload[protocol.RoomMoved](envelope)
		if err != nil {
			return
		}
		event.OwnerToken = value.OwnerToken
	case protocol.TypeChatMessage:
		value, err := protocol.DecodePayload[protocol.ChatMessage](envelope)
		if err != nil {
			return
		}
		event.Chat = &value
	case protocol.TypeActivityMessage:
		value, err := protocol.DecodePayload[protocol.ActivityMessage](envelope)
		if err != nil {
			return
		}
		event.Activity = &value
	case protocol.TypeError:
		value, err := protocol.DecodePayload[protocol.Error](envelope)
		if err != nil {
			return
		}
		event.Error = &value
	}
	c.emit(event)
}

func (c *Client) Snapshot() protocol.Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value := c.snapshot
	participants := make([]protocol.Participant, len(value.Participants))
	copy(participants, value.Participants)
	for i := range participants {
		participants[i].AvailableMedia = append([]string(nil), participants[i].AvailableMedia...)
		if participants[i].Media != nil {
			media := *participants[i].Media
			participants[i].Media = &media
		}
	}
	value.Participants = participants
	items := make([]protocol.PlaylistItem, len(value.Playlist.Items))
	copy(items, value.Playlist.Items)
	for i := range items {
		if items[i].Media != nil {
			media := *items[i].Media
			items[i].Media = &media
		}
	}
	value.Playlist.Items = items
	value.PlaylistWheel = cloneWheel(value.PlaylistWheel)
	value.StreamOffers = append([]protocol.MediaStreamOffer(nil), value.StreamOffers...)
	return value
}

func (c *Client) Welcome() protocol.Welcome {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.welcome
}

func (c *Client) Supports(capability protocol.Capability) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, current := range c.welcome.Capabilities {
		if current == capability {
			return true
		}
	}
	return false
}

func (c *Client) ServerNow() time.Time { return time.Now().Add(c.clock.offset()) }

func (c *Client) PlaybackPosition() float64 {
	c.mu.RLock()
	playback := c.snapshot.Playback
	c.mu.RUnlock()
	return playback.PositionAt(c.ServerNow())
}

func (c *Client) Events() <-chan Event { return c.events }

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) MoveRoom(value protocol.RoomMove) error {
	return c.command(protocol.TypeRoomMove, value)
}
func (c *Client) SetRoomMode(value protocol.RoomModeSet) error {
	return c.command(protocol.TypeRoomModeSet, value)
}
func (c *Client) SetRole(value protocol.RoomRoleSet) error {
	return c.command(protocol.TypeRoomRoleSet, value)
}
func (c *Client) SetPlayback(value protocol.PlaybackSet) error {
	return c.command(protocol.TypePlaybackSet, value)
}
func (c *Client) SetMedia(value protocol.MediaSet) error {
	return c.command(protocol.TypeMediaSet, value)
}
func (c *Client) SetPlaylist(value protocol.PlaylistSet) error {
	return c.command(protocol.TypePlaylistSet, value)
}
func (c *Client) SelectPlaylist(index int) error {
	return c.command(protocol.TypePlaylistSelect, protocol.PlaylistSelect{Index: index})
}
func (c *Client) SpinPlaylistWheel() error {
	return c.command(protocol.TypePlaylistWheelSpin, protocol.PlaylistWheelSpin{})
}
func (c *Client) SendChat(message string) error {
	return c.command(protocol.TypeChatSend, protocol.ChatSend{Message: message})
}
func (c *Client) PublishStreamOffer(value protocol.MediaStreamOfferPublish) error {
	return c.command(protocol.TypeStreamOfferPublish, value)
}
func (c *Client) WithdrawStreamOffer(offerID string) error {
	return c.command(protocol.TypeStreamOfferWithdraw, protocol.MediaStreamOfferWithdraw{OfferID: offerID})
}
func (c *Client) RequestStream(value protocol.MediaStreamRequest) (protocol.MediaStreamRequestAccepted, error) {
	envelope, err := c.commandEnvelope(protocol.TypeStreamRequest, value)
	if err != nil {
		return protocol.MediaStreamRequestAccepted{}, err
	}
	if envelope.Type != protocol.TypeStreamRequestAccepted {
		return protocol.MediaStreamRequestAccepted{}, errors.New("server returned an invalid stream request acknowledgement")
	}
	accepted, err := protocol.DecodePayload[protocol.MediaStreamRequestAccepted](envelope)
	if err != nil {
		return protocol.MediaStreamRequestAccepted{}, err
	}
	return accepted, nil
}
func (c *Client) GrantStream(value protocol.MediaStreamGrant) error {
	return c.command(protocol.TypeStreamGrant, value)
}
func (c *Client) RevokeStream(value protocol.MediaStreamRevoke) error {
	return c.command(protocol.TypeStreamRevoke, value)
}

func (c *Client) command(messageType protocol.MessageType, payload any) error {
	_, err := c.commandEnvelope(messageType, payload)
	return err
}

func (c *Client) commandEnvelope(messageType protocol.MessageType, payload any) (protocol.Envelope, error) {
	id := c.nextID()
	replies := make(chan commandReply, 1)
	c.pendingMu.Lock()
	c.pending[id] = replies
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	if err := c.write(messageType, id, payload); err != nil {
		return protocol.Envelope{}, err
	}
	timer := time.NewTimer(commandTimeout)
	defer timer.Stop()
	select {
	case reply := <-replies:
		if reply.err != nil {
			return protocol.Envelope{}, reply.err
		}
		if reply.envelope.Type == protocol.TypeError {
			failure, err := protocol.DecodePayload[protocol.Error](reply.envelope)
			if err != nil {
				return protocol.Envelope{}, err
			}
			return protocol.Envelope{}, fmt.Errorf("%s: %s", failure.Code, failure.Message)
		}
		return reply.envelope, nil
	case <-timer.C:
		return protocol.Envelope{}, errors.New("server did not acknowledge command")
	case <-c.done:
		return protocol.Envelope{}, errors.New("connection closed before command acknowledgement")
	}
}

func (c *Client) deliverReply(envelope protocol.Envelope) bool {
	c.pendingMu.Lock()
	replies := c.pending[envelope.ReplyTo]
	c.pendingMu.Unlock()
	if replies == nil {
		return false
	}
	select {
	case replies <- commandReply{envelope: envelope}:
	default:
	}
	return true
}

func (c *Client) write(messageType protocol.MessageType, id string, payload any) error {
	envelope, err := protocol.NewEnvelope(messageType, id, "", payload)
	if err != nil {
		return err
	}
	return c.codec.Write(envelope)
}

func (c *Client) pingLoop() {
	_ = c.write(protocol.TypePing, c.nextID(), protocol.Ping{ClientTimeUnixMs: time.Now().UnixMilli()})
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = c.write(protocol.TypePing, c.nextID(), protocol.Ping{ClientTimeUnixMs: time.Now().UnixMilli()})
		case <-c.done:
			return
		}
	}
}

func (c *Client) nextID() string { return fmt.Sprintf("c-%d", c.seq.Add(1)) }

func (c *Client) emit(event Event) {
	select {
	case c.events <- event:
	case <-c.done:
	default:
	}
}

func (c *Client) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		err = c.conn.Close()
		c.pendingMu.Lock()
		for _, replies := range c.pending {
			select {
			case replies <- commandReply{err: errors.New("connection closed")}:
			default:
			}
		}
		c.pendingMu.Unlock()
	})
	return err
}

func cloneWheel(wheel *protocol.PlaylistWheel) *protocol.PlaylistWheel {
	if wheel == nil {
		return nil
	}
	value := *wheel
	value.Items = append([]protocol.PlaylistWheelItem(nil), wheel.Items...)
	return &value
}

func (c *Client) SetMediaAvailability(value protocol.MediaAvailabilitySet) error {
	return c.command(protocol.TypeMediaAvailabilitySet, value)
}
