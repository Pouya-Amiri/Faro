package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	faroserver "github.com/Pouya-Amiri/Faro/internal/server"
	tailcattransport "github.com/Pouya-Amiri/Faro/internal/streamtransport/tailcat"
)

func (s *Service) StartServer(request ServerRequest) (LocalServerStatus, error) {
	if request.Mode != "" && request.Mode != "peer" && request.Mode != "advanced" {
		return LocalServerStatus{}, errors.New("unknown hosting mode")
	}
	peer := request.Mode != "advanced"
	if peer {
		request.ListenAddress = "127.0.0.1:0"
		request.PublicHost = "localhost"
		request.Protected = true
	}
	request.ListenAddress = strings.TrimSpace(request.ListenAddress)
	request.PublicHost = strings.TrimSpace(request.PublicHost)
	request.Room = strings.TrimSpace(request.Room)
	if request.ListenAddress == "" {
		request.ListenAddress = ":8999"
	}
	if request.PublicHost == "" {
		request.PublicHost = "localhost"
	}
	if request.Room == "" {
		request.Room = "watch"
	}
	s.mu.Lock()
	if s.localServer != nil || s.serverStarting {
		s.mu.Unlock()
		return LocalServerStatus{}, errors.New("a local Faro server is already running")
	}
	ctx, cancel := context.WithCancel(s.root)
	s.serverCancel = cancel
	s.serverStarting = true
	s.mu.Unlock()
	started := false
	defer func() {
		if started {
			return
		}
		s.mu.Lock()
		s.serverStarting = false
		s.serverCancel = nil
		s.mu.Unlock()
		cancel()
	}()
	listener, err := net.Listen("tcp", request.ListenAddress)
	if err != nil {
		return LocalServerStatus{}, fmt.Errorf("start local server: %w", err)
	}
	joinToken := ""
	if request.Protected {
		joinToken, err = secureToken()
		if err != nil {
			_ = listener.Close()
			return LocalServerStatus{}, err
		}
	}
	cfg := faroserver.DefaultConfig()
	cfg.Address = listener.Addr().String()
	cfg.JoinToken = joinToken
	cfg.InviteHost = request.PublicHost
	cfg.InviteRoom = request.Room
	cfg.PrintInvite = true
	cfg.StreamingDERPMapURL = strings.TrimSpace(request.StreamingDERPMapURL)
	cfg.StreamingEnabled = !request.DisableStreaming
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := faroserver.New(cfg, buildinfo.EffectiveVersion(), logger)
	if err != nil {
		_ = listener.Close()
		return LocalServerStatus{}, err
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return LocalServerStatus{}, err
	}
	localURL, err := invite.Format(invite.Invite{
		Address: net.JoinHostPort(localInviteHost(listener.Addr()), port), Room: request.Room,
		Fingerprint: server.Fingerprint(), JoinToken: joinToken,
	})
	if err != nil {
		_ = listener.Close()
		return LocalServerStatus{}, err
	}
	shareInvite := server.InviteURL()
	if peer {
		endpoint, startErr := tailcattransport.StartRoom(ctx, listener.Addr().String())
		if startErr != nil {
			cancel()
			_ = listener.Close()
			return LocalServerStatus{}, startErr
		}
		shared, _ := invite.Parse(localURL)
		shared.Address = "peer:8999"
		shared.Tailcat = endpoint
		shareInvite, err = invite.Format(shared)
		if err != nil {
			cancel()
			_ = listener.Close()
			return LocalServerStatus{}, err
		}
	}
	status := LocalServerStatus{
		Running: true, ListenAddress: listener.Addr().String(), ShareInvite: shareInvite,
		LocalInvite: localURL, Fingerprint: server.Fingerprint(),
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		_ = listener.Close()
		return LocalServerStatus{}, ctx.Err()
	}
	s.localServer, s.serverCancel, s.serverStatus, s.serverStarting = server, cancel, status, false
	s.mu.Unlock()
	started = true
	go func() {
		defer cancel()
		err := server.ServeListener(ctx, listener)
		server.Wait()
		s.mu.Lock()
		if s.localServer == server {
			s.localServer, s.serverCancel = nil, nil
			s.serverStatus.Running = false
		}
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "local_server", Message: err.Error(), Fatal: true}})
		}
	}()
	return status, nil
}

func (s *Service) ServerStatus() LocalServerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.serverStatus
}

func (s *Service) StopServer() {
	s.mu.Lock()
	server, cancel := s.localServer, s.serverCancel
	s.localServer, s.serverCancel = nil, nil
	s.serverStatus.Running = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if server != nil {
		_ = server.Close()
	}
}

func secureToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func localInviteHost(listenerAddr net.Addr) string {
	if listenerAddr == nil {
		return "localhost"
	}
	host, _, err := net.SplitHostPort(listenerAddr.String())
	if err != nil {
		return "localhost"
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "0.0.0.0", "::":
		return "localhost"
	default:
		return host
	}
}
