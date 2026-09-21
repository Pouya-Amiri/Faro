package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

type Server struct {
	cfg         Config
	version     string
	log         *slog.Logger
	identity    *TLSIdentity
	fingerprint string
	inviteURL   string
	hub         *hub
	streaming   *protocol.MediaStreamPolicy

	joinTokenDigest [sha256.Size]byte
	hasJoinToken    bool

	mu        sync.Mutex
	listeners []net.Listener
	sessions  map[*session]struct{}
	byIP      map[string]int
	wait      sync.WaitGroup
	closeOnce sync.Once
}

func New(cfg Config, version string, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	identity, fingerprint, err := LoadOrCreateTLSIdentity(cfg.TLSDir)
	if err != nil {
		return nil, fmt.Errorf("prepare TLS identity: %w", err)
	}
	var streaming *protocol.MediaStreamPolicy
	if cfg.StreamingEnabled {
		streaming = &protocol.MediaStreamPolicy{
			MaxOffersPerParticipant: cfg.MaxStreamOffersPerParticipant,
			MaxViewersPerOffer:      cfg.MaxStreamViewersPerOffer,
			GrantTTLSeconds:         int(cfg.StreamGrantTTL.Seconds()),
			DERPMapURL:              cfg.StreamingDERPMapURL,
		}
	}
	server := &Server{
		cfg: cfg, version: version, log: logger, identity: identity,
		fingerprint: fingerprint,
		hub:         newHub(cfg.MaxRooms, cfg.MaxRoomParticipants, streaming), streaming: streaming,
		sessions: make(map[*session]struct{}), byIP: make(map[string]int),
	}
	if cfg.PrintInvite {
		address, err := publicAddress(cfg.InviteHost, cfg.Address)
		if err != nil {
			return nil, err
		}
		server.inviteURL, err = invite.Format(invite.Invite{
			Address: address, Room: cfg.InviteRoom, Fingerprint: fingerprint, JoinToken: cfg.JoinToken,
		})
		if err != nil {
			return nil, fmt.Errorf("format invite: %w", err)
		}
	}
	if cfg.JoinToken != "" {
		server.joinTokenDigest = tokenDigest(cfg.JoinToken)
		server.hasJoinToken = true
		server.cfg.JoinToken = ""
	}
	return server, nil
}

func (s *Server) Fingerprint() string { return s.fingerprint }

func (s *Server) InviteURL() string { return s.inviteURL }

func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Address, err)
	}
	return s.ServeListener(ctx, listener)
}

func (s *Server) ServeListener(ctx context.Context, listener net.Listener) error {
	tlsListener := tls.NewListener(listener, s.identity.Config())
	s.mu.Lock()
	s.listeners = append(s.listeners, tlsListener)
	s.mu.Unlock()
	s.log.Info("Faro server listening", "address", listener.Addr().String(), "version", s.version, "tls", "1.3", "fingerprint", s.fingerprint)
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	for {
		connection, err := tlsListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		remoteIP := addressIP(connection.RemoteAddr())
		if !s.admit(remoteIP) {
			s.log.Warn("connection limit reached", "remote_ip", remoteIP)
			_ = connection.Close()
			continue
		}
		client, err := newSession(s, connection)
		if err != nil {
			s.release(remoteIP)
			_ = connection.Close()
			continue
		}
		s.mu.Lock()
		s.sessions[client] = struct{}{}
		s.mu.Unlock()
		s.wait.Add(1)
		go func() {
			defer s.wait.Done()
			defer func() {
				s.mu.Lock()
				delete(s.sessions, client)
				s.byIP[remoteIP]--
				if s.byIP[remoteIP] == 0 {
					delete(s.byIP, remoteIP)
				}
				s.mu.Unlock()
			}()
			client.run()
		}()
	}
}

func (s *Server) authenticate(token string) bool {
	if !s.hasJoinToken {
		return true
	}
	return tokenMatches(s.joinTokenDigest, token)
}

func (s *Server) Close() error {
	var result error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		for _, listener := range s.listeners {
			result = errors.Join(result, listener.Close())
		}
		for client := range s.sessions {
			client.close()
		}
		s.mu.Unlock()
	})
	return result
}

func (s *Server) Wait() { s.wait.Wait() }

func (s *Server) admit(remoteIP string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= s.cfg.MaxConnections || s.byIP[remoteIP] >= s.cfg.MaxConnectionsPerIP {
		return false
	}
	s.byIP[remoteIP]++
	return true
}

func (s *Server) release(remoteIP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byIP[remoteIP]--
	if s.byIP[remoteIP] <= 0 {
		delete(s.byIP, remoteIP)
	}
}

func addressIP(address net.Addr) string {
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return address.String()
	}
	return host
}

func publicAddress(publicHost, listenAddress string) (string, error) {
	if parsed, err := url.Parse("//" + strings.TrimSpace(publicHost)); err == nil && parsed.Hostname() != "" && parsed.Port() != "" {
		return net.JoinHostPort(parsed.Hostname(), parsed.Port()), nil
	}
	_, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return "", fmt.Errorf("derive invite port: %w", err)
	}
	host := strings.Trim(strings.TrimSpace(publicHost), "[]")
	if host == "" {
		return "", errors.New("invite host is required")
	}
	return net.JoinHostPort(host, port), nil
}
