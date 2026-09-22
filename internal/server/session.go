package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

const (
	helloTimeout = 10 * time.Second
	idleTimeout  = 30 * time.Second
	writeTimeout = 5 * time.Second
)

type outbound struct {
	envelope protocol.Envelope
	done     chan error
}

type rateWindow struct {
	started time.Time
	count   int
	maximum int
	window  time.Duration
}

func (r *rateWindow) allow(now time.Time) bool {
	if r.started.IsZero() || now.Sub(r.started) >= r.window {
		r.started = now
		r.count = 0
	}
	if r.count >= r.maximum {
		return false
	}
	r.count++
	return true
}

type session struct {
	id     string
	server *Server
	log    *slog.Logger
	conn   net.Conn
	codec  *protocol.Codec
	out    chan outbound
	done   chan struct{}
	once   sync.Once

	participant  *participant
	capabilities map[protocol.Capability]struct{}
	commands     rateWindow
	chat         rateWindow
}

func newSession(server *Server, connection net.Conn) (*session, error) {
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &session{
		id: id, server: server,
		log:  server.log.With("remote", connection.RemoteAddr().String(), "session", id),
		conn: connection, codec: protocol.NewCodec(connection),
		out: make(chan outbound, 128), done: make(chan struct{}),
		capabilities: make(map[protocol.Capability]struct{}),
		commands:     rateWindow{maximum: 120, window: 10 * time.Second},
		chat:         rateWindow{maximum: 8, window: 10 * time.Second},
	}, nil
}

func (s *session) run() {
	go s.writeLoop()
	defer func() {
		s.close()
		s.server.hub.leave(s.participant)
	}()
	_ = s.conn.SetReadDeadline(time.Now().Add(helloTimeout))
	first, err := s.codec.Read()
	if err != nil {
		s.fatal("invalid_frame", err.Error())
		return
	}
	if first.Type != protocol.TypeHello {
		s.fatal("hello_required", "the first protocol message must be hello")
		return
	}
	if err := s.handleHello(first); err != nil {
		failure := asCommandError(err)
		s.fatal(failure.code, failure.message)
		return
	}
	_ = s.conn.SetReadDeadline(time.Now().Add(idleTimeout))
	for {
		envelope, err := s.codec.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Debug("connection read ended", "error", err)
			}
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if envelope.Type == protocol.TypeHello {
			s.commandFailure(envelope.ID, invalid("already_joined", "hello may only be sent once"))
			continue
		}
		if !s.commands.allow(time.Now()) {
			s.commandFailure(envelope.ID, invalid("rate_limited", "too many commands; slow down"))
			continue
		}
		if err := s.handle(envelope); err != nil {
			s.commandFailure(envelope.ID, err)
		} else if envelope.Type != protocol.TypePing && envelope.Type != protocol.TypeStreamRequest {
			s.sendMessage(protocol.TypeCommandOK, "", envelope.ID, protocol.CommandOK{})
		}
	}
}

func (s *session) handleHello(envelope protocol.Envelope) error {
	hello, err := protocol.DecodePayload[protocol.Hello](envelope)
	if err != nil {
		return invalid("invalid_hello", "hello payload is invalid")
	}
	if !s.server.authenticate(hello.JoinToken) {
		return invalid("unauthorized", "the server join token is missing or invalid")
	}
	if err := s.negotiateCapabilities(hello.Capabilities); err != nil {
		return err
	}
	result, err := s.server.hub.join(s, hello)
	if err != nil {
		return err
	}
	s.participant = result.participant
	welcome := protocol.Welcome{
		ServerVersion: s.server.version,
		SessionID:     s.id,
		ParticipantID: result.participant.state.ID,
		RoomID:        result.room.state.ID,
		OwnerToken:    result.ownerToken,
		Capabilities:  s.negotiatedCapabilities(),
	}
	if s.supports(protocol.CapabilityMediaStreamV1) {
		policy := *s.server.streaming
		welcome.Streaming = &policy
	}
	s.sendMessage(protocol.TypeWelcome, "", envelope.ID, welcome)
	snapshot, err := s.server.hub.snapshot(result.participant)
	if err != nil {
		return err
	}
	participants, err := s.server.hub.participants(result.participant)
	if err != nil {
		return err
	}
	s.sendMessage(protocol.TypeStateSnapshot, "", "", snapshot)
	broadcast(result.existing, protocol.TypeParticipantsUpdated, participants)
	return nil
}

func (s *session) handle(envelope protocol.Envelope) error {
	switch envelope.Type {
	case protocol.TypePing:
		request, err := decode[protocol.Ping](envelope)
		if err != nil {
			return err
		}
		s.sendMessage(protocol.TypePong, "", envelope.ID, protocol.Pong{
			ClientTimeUnixMs: request.ClientTimeUnixMs,
			ServerTimeUnixMs: time.Now().UnixMilli(),
		})
		return nil
	case protocol.TypeRoomModeSet:
		request, err := decode[protocol.RoomModeSet](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.setMode(s.participant, request)
	case protocol.TypeRoomRoleSet:
		request, err := decode[protocol.RoomRoleSet](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.setRole(s.participant, request)
	case protocol.TypePlaybackSet:
		request, err := decode[protocol.PlaybackSet](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.setPlayback(s.participant, request)
	case protocol.TypeMediaAvailabilitySet:
		request, err := decode[protocol.MediaAvailabilitySet](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.setMediaAvailability(s.participant, request)
	case protocol.TypeMediaSet:
		request, err := decode[protocol.MediaSet](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.setMedia(s.participant, request)
	case protocol.TypePlaylistSet:
		request, err := decode[protocol.PlaylistSet](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.setPlaylist(s.participant, request)
	case protocol.TypePlaylistSelect:
		request, err := decode[protocol.PlaylistSelect](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.selectPlaylist(s.participant, request.Index)
	case protocol.TypePlaylistWheelSpin:
		if _, err := decode[protocol.PlaylistWheelSpin](envelope); err != nil {
			return err
		}
		return s.server.hub.spinPlaylistWheel(s.participant)
	case protocol.TypeChatSend:
		if !s.chat.allow(time.Now()) {
			return invalid("chat_rate_limited", "too many chat messages; slow down")
		}
		request, err := decode[protocol.ChatSend](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.chat(s.participant, request)
	case protocol.TypeStreamOfferPublish:
		request, err := decode[protocol.MediaStreamOfferPublish](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.publishStreamOffer(s.participant, request)
	case protocol.TypeStreamOfferWithdraw:
		request, err := decode[protocol.MediaStreamOfferWithdraw](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.withdrawStreamOffer(s.participant, request.OfferID)
	case protocol.TypeStreamRequest:
		request, err := decode[protocol.MediaStreamRequest](envelope)
		if err != nil {
			return err
		}
		accepted, err := s.server.hub.requestStream(s.participant, request)
		if err != nil {
			return err
		}
		s.sendMessage(protocol.TypeStreamRequestAccepted, "", envelope.ID, accepted)
		return nil
	case protocol.TypeStreamGrant:
		request, err := decode[protocol.MediaStreamGrant](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.grantStream(s.participant, request)
	case protocol.TypeStreamRevoke:
		request, err := decode[protocol.MediaStreamRevoke](envelope)
		if err != nil {
			return err
		}
		return s.server.hub.revokeStream(s.participant, request)
	default:
		return invalid("unknown_message", fmt.Sprintf("unknown client message type %q", envelope.Type))
	}
}

func (s *session) negotiateCapabilities(requested []protocol.Capability) error {
	if len(requested) > 32 {
		return invalid("invalid_hello", "too many client capabilities")
	}
	seen := make(map[protocol.Capability]struct{}, len(requested))
	for _, capability := range requested {
		if capability == "" || len(capability) > 64 || strings.ContainsAny(string(capability), "\r\n\x00") {
			return invalid("invalid_hello", "client capability is invalid")
		}
		if _, duplicate := seen[capability]; duplicate {
			return invalid("invalid_hello", "client capabilities must be unique")
		}
		seen[capability] = struct{}{}
		if capability == protocol.CapabilityMediaAvailabilityV1 || capability == protocol.CapabilityMediaStreamV1 && s.server.streaming != nil {
			s.capabilities[capability] = struct{}{}
		}
	}
	return nil
}

func (s *session) supports(capability protocol.Capability) bool {
	if s == nil {
		return false
	}
	_, ok := s.capabilities[capability]
	return ok
}

func (s *session) negotiatedCapabilities() []protocol.Capability {
	result := make([]protocol.Capability, 0, len(s.capabilities))
	for capability := range s.capabilities {
		result = append(result, capability)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func decode[T any](envelope protocol.Envelope) (T, error) {
	value, err := protocol.DecodePayload[T](envelope)
	if err != nil {
		return value, invalid("invalid_payload", fmt.Sprintf("invalid payload for %s", envelope.Type))
	}
	return value, nil
}

func (s *session) commandFailure(replyTo string, err error) {
	failure := asCommandError(err)
	s.sendMessage(protocol.TypeError, "", replyTo, protocol.Error{Code: failure.code, Message: failure.message})
}

func (s *session) fatal(code, message string) {
	envelope, err := protocol.NewEnvelope(protocol.TypeError, "", "", protocol.Error{Code: code, Message: message, Fatal: true})
	if err != nil {
		return
	}
	done := make(chan error, 1)
	select {
	case s.out <- outbound{envelope: envelope, done: done}:
		select {
		case <-done:
		case <-time.After(writeTimeout):
		}
	case <-s.done:
	}
}

func (s *session) sendMessage(messageType protocol.MessageType, id, replyTo string, payload any) {
	envelope, err := protocol.NewEnvelope(messageType, id, replyTo, payload)
	if err != nil {
		s.log.Error("could not encode protocol message", "type", messageType, "error", err)
		return
	}
	select {
	case s.out <- outbound{envelope: envelope}:
	case <-s.done:
	default:
		s.log.Warn("closing slow client")
		s.close()
	}
}

func (s *session) writeLoop() {
	for {
		select {
		case frame := <-s.out:
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := s.codec.Write(frame.envelope)
			if frame.done != nil {
				frame.done <- err
			}
			if err != nil {
				s.close()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

func tokenDigest(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

func tokenMatches(expected [sha256.Size]byte, actual string) bool {
	digest := tokenDigest(actual)
	return hmac.Equal(expected[:], digest[:])
}
