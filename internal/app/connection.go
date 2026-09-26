package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/invite"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	tailcattransport "github.com/Pouya-Amiri/Faro/internal/streamtransport/tailcat"
	"github.com/Pouya-Amiri/Faro/internal/syncer"
)

func (s *Service) Connect(request ConnectionRequest) error {
	parsed, err := invite.Parse(request.Invite)
	if err != nil {
		return err
	}
	if strings.TrimSpace(request.Name) == "" {
		return errors.New("name is required")
	}
	if err := validatePlayer(request.Player); err != nil {
		return err
	}
	if request.ClientVersion == "" {
		request.ClientVersion = buildinfo.EffectiveVersion()
	}
	s.Disconnect()
	ctx, cancel := context.WithCancel(s.root)
	client, err := faroclient.Connect(ctx, faroclient.Config{
		DialContext: tailcattransport.RoomDialer(parsed.Tailcat),
		Address:     parsed.Address, Fingerprint: parsed.Fingerprint,
		JoinToken: parsed.JoinToken, OwnerToken: parsed.OwnerToken,
		Name: request.Name, Room: parsed.Room, ClientVersion: request.ClientVersion,
		Capabilities: []protocol.Capability{protocol.CapabilityMediaStreamV1, protocol.CapabilityMediaAvailabilityV1},
	})
	if err != nil {
		cancel()
		return err
	}
	welcome := client.Welcome()
	s.mu.Lock()
	s.sessionCtx, s.cancel, s.client, s.player, s.sync = ctx, cancel, client, nil, nil
	s.playerCancel = nil
	s.playerContext = nil
	s.playerDismissed = false
	s.playerCloseGeneration = 0
	s.wheelID = ""
	s.invite = parsed
	s.ownerToken = welcome.OwnerToken
	if s.ownerToken == "" {
		s.ownerToken = parsed.OwnerToken
	}
	s.lastPlayer = player.State{}
	s.lastRemoteRevision = 0
	s.expectedPause, s.expectedSeek, s.expectedRate = nil, nil, nil
	s.selectedItem, s.selectedItemIdentity = "", ""
	s.currentSource = ""
	s.manualSource = ""
	s.sponsorSegments = nil
	s.lastSponsorEnd = 0
	s.openingMedia = false
	s.transitionPaused = nil
	s.localPlaybackPending = false
	s.request = request
	s.mu.Unlock()
	go s.consumeClient(ctx, client)
	go s.applySelectedPlaylist(ctx, client)
	go s.reconcilePlaylistStreams(ctx, client)
	go s.syncLoop(ctx)
	s.emitConnection("connected", 0, "")
	s.emitSnapshot()
	return nil
}

func (s *Service) consumeClient(ctx context.Context, client *faroclient.Client) {
	playlistUpdates := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-playlistUpdates:
				s.applySelectedPlaylist(ctx, client)
			case <-client.Done():
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case event := <-client.Events():
			if event.Type == protocol.TypePong {
				// Clock samples change no room state; re-rendering the whole UI
				// every ping interval only causes flicker.
				continue
			}
			if event.StreamRequest != nil {
				go s.handleStreamRequest(ctx, client, *event.StreamRequest)
			}
			if event.StreamGrant != nil {
				go s.activateStream(ctx, client, *event.StreamGrant)
			}
			if event.StreamRevoked != nil {
				s.handleStreamRevoked(client, *event.StreamRevoked)
			}
			if event.Type == protocol.TypePlaybackUpdated {
				snapshot := client.Snapshot()
				var err error
				if s.shouldApplyRemote(snapshot) {
					err = s.applyRemote(ctx, snapshot.Playback, snapshot.Playback.Seek)
				}
				if err != nil {
					s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "player_sync", Message: err.Error()}})
				}
			}
			if event.Type == protocol.TypePlaylistUpdated || event.Type == protocol.TypeStateSnapshot {
				select {
				case playlistUpdates <- struct{}{}:
				default:
				}
			}
			if event.Type == protocol.TypePlaylistUpdated || event.Type == protocol.TypeStateSnapshot || event.Type == protocol.TypeStreamOffersUpdated {
				go s.reconcilePlaylistStreams(ctx, client)
			}
			switch event.Type {
			case protocol.TypePlaylistUpdated, protocol.TypeStateSnapshot, protocol.TypeStreamOffersUpdated,
				protocol.TypeMediaUpdated, protocol.TypeParticipantsUpdated:
				s.scheduleAutoOffer(ctx, client)
			}
			if event.Chat != nil {
				s.sink(Event{Kind: "chat", Chat: event.Chat})
				s.showChatInPlayer(event.Chat)
			}
			if event.Activity != nil {
				s.sink(Event{Kind: "activity", Activity: event.Activity})
			}
			if event.Error != nil {
				s.sink(Event{Kind: "error", Error: event.Error})
			}
			if event.Wheel != nil {
				s.applyWheelPlaybackIntent(*event.Wheel)
				s.sink(Event{Kind: "wheel", Wheel: event.Wheel, ServerNowUnixMs: client.ServerNow().UnixMilli()})
			}
			if changesRoomState(event.Type) {
				s.emitSnapshot()
			}
		case <-client.Done():
			if ctx.Err() == nil {
				s.reconnect(ctx, client)
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) applyRemote(ctx context.Context, playback protocol.Playback, forceSeek bool) error {
	s.syncApplyMu.Lock()
	defer s.syncApplyMu.Unlock()
	s.mu.RLock()
	controller := s.sync
	blocked := s.openingMedia || s.localPlaybackPending
	s.mu.RUnlock()
	if blocked {
		return nil
	}
	if controller == nil {
		return nil
	}
	applyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := controller.ApplyRemote(applyCtx, playback, forceSeek)
	if err == nil {
		s.mu.Lock()
		s.lastRemoteRevision = playback.Revision
		s.mu.Unlock()
	}
	return err
}

func (s *Service) shouldApplyRemote(snapshot protocol.Snapshot) bool {
	s.mu.RLock()
	blocked := s.openingMedia || s.localPlaybackPending
	controller := s.sync
	s.mu.RUnlock()
	return !blocked && controller != nil && mediaMatchesController(snapshot)
}

func (s *Service) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		select {
		case <-ticker.C:
			client, _ := s.currentConnection()
			if client == nil {
				continue
			}
			snapshot := client.Snapshot()
			if !s.shouldApplyRemote(snapshot) {
				s.reportSync(SyncStatus{State: "idle"})
				continue
			}
			s.mu.RLock()
			force := snapshot.Playback.Seek && snapshot.Playback.SetBy != snapshot.SelfID && snapshot.Playback.Revision != s.lastRemoteRevision
			controller := s.sync
			s.mu.RUnlock()
			err := s.applyRemote(ctx, snapshot.Playback, force)
			if controller != nil {
				s.reportSync(syncStatusOf(controller.Status()))
			}
			if err != nil && err.Error() != lastError {
				lastError = err.Error()
				s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "player_sync", Message: lastError}})
			} else if err == nil {
				lastError = ""
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) reconnect(ctx context.Context, failed *faroclient.Client) {
	s.mu.Lock()
	if s.client != failed || s.sessionCtx != ctx {
		s.mu.Unlock()
		return
	}
	streams := s.detachStreamingLocked()
	streamInterrupted := streams.gateway != nil
	mediaPlayer := s.player
	// A failed client is no longer an authoritative snapshot source. Clear both
	// pointers before backoff so the player loop cannot repeatedly apply the
	// final state cached by the closed connection.
	s.client = nil
	s.sync = nil
	s.localPlaybackPending = false
	s.mu.Unlock()
	streams.close()
	if streamInterrupted && mediaPlayer != nil {
		pauseCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = mediaPlayer.SetPaused(pauseCtx, true)
		cancel()
	}
	for attempt := 1; ; attempt++ {
		s.mu.RLock()
		if s.sessionCtx != ctx || s.client != nil {
			s.mu.RUnlock()
			return
		}
		request, parsed, ownerToken := s.request, s.invite, s.ownerToken
		s.mu.RUnlock()
		delay := reconnectDelay(attempt)
		s.emitConnection("reconnecting", attempt, fmt.Sprintf("retrying in %s", delay))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		client, err := faroclient.Connect(ctx, faroclient.Config{
			DialContext: tailcattransport.RoomDialer(parsed.Tailcat),
			Address:     parsed.Address, Fingerprint: parsed.Fingerprint,
			JoinToken: parsed.JoinToken, OwnerToken: ownerToken,
			Name: request.Name, Room: parsed.Room, ClientVersion: request.ClientVersion,
			Capabilities: []protocol.Capability{protocol.CapabilityMediaStreamV1, protocol.CapabilityMediaAvailabilityV1},
		})
		if err != nil {
			if message, permanent := permanentConnectFailure(err); permanent {
				s.emitConnection("disconnected", attempt, message)
				s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "connection_rejected", Message: message}})
				return
			}
			s.emitConnection("reconnecting", attempt, err.Error())
			continue
		}
		welcome := client.Welcome()
		s.mu.Lock()
		if s.sessionCtx != ctx || s.client != nil || ctx.Err() != nil {
			s.mu.Unlock()
			_ = client.Close()
			return
		}
		s.client = client
		mediaPlayer := s.player
		if mediaPlayer != nil {
			s.sync = syncer.New(&trackedPlayer{Player: mediaPlayer, service: s}, client, client.ServerNow)
		} else {
			s.sync = nil
		}
		if welcome.OwnerToken != "" {
			s.ownerToken = welcome.OwnerToken
		}
		s.mu.Unlock()
		if mediaPlayer != nil && !streamInterrupted {
			stateCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			state, stateErr := mediaPlayer.State(stateCtx)
			cancel()
			if stateErr == nil && state.Source != "" {
				s.mu.RLock()
				source := s.currentSource
				s.mu.RUnlock()
				if source == "" {
					source = state.Source
				}
				if media, identityErr := s.identifyAndRemember(source, state.Title, state.DurationSeconds); identityErr == nil {
					_ = client.SetMedia(protocol.MediaSet{Media: media})
				}
			}
		}
		s.emitConnection("connected", attempt, "connection restored")
		if streamInterrupted {
			s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_unavailable", Message: "The connection changed, so the media stream must be requested again."}})
		}
		s.emitSnapshot()
		go s.consumeClient(ctx, client)
		go s.applySelectedPlaylist(ctx, client)
		go s.reconcilePlaylistStreams(ctx, client)
		return
	}
}

func (s *Service) currentConnection() (*faroclient.Client, *syncer.Controller) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client, s.sync
}

func (s *Service) emitConnection(state string, attempt int, message string) {
	s.sink(Event{Kind: "connection", Connection: &ConnectionStatus{State: state, Attempt: attempt, Message: message}})
}

func (s *Service) connected() (*faroclient.Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return nil, errors.New("not connected")
	}
	return s.client, nil
}

func (s *Service) connectedPlayer() (*faroclient.Client, player.Player, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil || s.player == nil {
		return nil, nil, errors.New("no media player is open")
	}
	return s.client, s.player, nil
}

type playerStartIntent uint8

const (
	playerStartAutomatic playerStartIntent = iota
	playerStartExplicit
)

var errPlayerDismissed = errors.New("player was closed; press Play to open it again")

func (s *Service) applyWheelPlaybackIntent(wheel protocol.PlaylistWheel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch wheel.Phase {
	case protocol.PlaylistWheelStarted:
		s.wheelID = wheel.ID
		s.wheelCloseGeneration = s.playerCloseGeneration
	case protocol.PlaylistWheelCompleted:
		if s.wheelID != wheel.ID || s.wheelCloseGeneration == s.playerCloseGeneration {
			s.playerDismissed = false
		}
		s.wheelID = ""
	case protocol.PlaylistWheelCancelled:
		if s.wheelID == wheel.ID {
			s.wheelID = ""
		}
	}
}

func (s *Service) ensurePlayer(intent playerStartIntent) (*faroclient.Client, player.Player, error) {
	s.playerLifecycleMu.Lock()
	defer s.playerLifecycleMu.Unlock()

	s.mu.RLock()
	client, mediaPlayer, sessionCtx, request := s.client, s.player, s.sessionCtx, s.request
	dismissed := s.playerDismissed
	s.mu.RUnlock()
	if client == nil || sessionCtx == nil {
		return nil, nil, errors.New("not connected")
	}
	if mediaPlayer == nil && dismissed && intent == playerStartAutomatic {
		return nil, nil, errPlayerDismissed
	}
	if mediaPlayer != nil {
		checkCtx, cancel := context.WithTimeout(sessionCtx, time.Second)
		_, err := mediaPlayer.State(checkCtx)
		cancel()
		if err == nil {
			return client, mediaPlayer, nil
		}
		s.mu.Lock()
		oldCancel := s.playerCancel
		s.player, s.playerCancel, s.sync = nil, nil, nil
		s.playerContext = nil
		s.playerDismissed = intent == playerStartAutomatic
		s.selectedItem, s.selectedItemIdentity = "", ""
		s.mu.Unlock()
		if oldCancel != nil {
			oldCancel()
		}
		_ = mediaPlayer.Close()
		if intent == playerStartAutomatic {
			return nil, nil, errPlayerDismissed
		}
	}
	s.mu.Lock()
	s.playerDismissed = false
	s.mu.Unlock()
	playerCtx, playerCancel := context.WithCancel(sessionCtx)
	started, err := s.startPlayer(playerCtx, request)
	if err != nil {
		playerCancel()
		return nil, nil, err
	}
	s.mu.Lock()
	if s.client != client || s.sessionCtx != sessionCtx {
		s.mu.Unlock()
		playerCancel()
		_ = started.Close()
		return nil, nil, errors.New("connection changed while starting player")
	}
	s.player, s.playerCancel = started, playerCancel
	s.playerContext = playerCtx
	s.playerDismissed = false
	s.sync = syncer.New(&trackedPlayer{Player: started, service: s}, client, client.ServerNow)
	s.lastPlayer = player.State{}
	s.lastRemoteRevision = 0
	s.expectedPause, s.expectedSeek, s.expectedRate = nil, nil, nil
	s.mu.Unlock()
	go s.consumePlayer(playerCtx, started)
	return client, started, nil
}

func (s *Service) releasePlayer(target player.Player, message string) {
	s.releasePlayerWithDismissal(target, message, true)
}

func (s *Service) releasePlayerWithDismissal(target player.Player, message string, dismissed bool) {
	s.playerLifecycleMu.Lock()
	s.mu.Lock()
	if s.player != target {
		s.mu.Unlock()
		s.playerLifecycleMu.Unlock()
		return
	}
	client, cancel := s.client, s.playerCancel
	s.player, s.playerCancel, s.sync = nil, nil, nil
	s.playerContext = nil
	s.playerDismissed = dismissed
	if dismissed {
		s.playerCloseGeneration++
	}
	s.selectedItem, s.selectedItemIdentity = "", ""
	if !dismissed {
		s.currentSource, s.currentPlayerSource = "", ""
		s.manualSource = ""
	}
	s.localPlaybackPending = false
	s.lastPlayer = player.State{}
	s.lastRemoteRevision = 0
	s.expectedPause, s.expectedSeek, s.expectedRate = nil, nil, nil
	s.openingMedia = false
	s.transitionPaused = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = target.Close()
	defer s.playerLifecycleMu.Unlock()
	if client != nil {
		snapshot := localClockSnapshot(client)
		if canControlPlayback(snapshot) && !snapshot.Playback.Paused {
			_ = client.SetPlayback(protocol.PlaybackSet{
				PositionSeconds: snapshot.Playback.PositionSeconds, Paused: true,
				Rate: normalizedRate(snapshot.Playback.Rate),
			})
		}
		_ = client.SetMedia(protocol.MediaSet{Media: nil})
	}
	if message != "" {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "player_closed", Message: message}})
	}
}

func (s *Service) emitSnapshot() {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		return
	}
	snapshot := localClockSnapshot(client)
	s.sink(Event{Kind: "snapshot", Snapshot: &snapshot, ServerNowUnixMs: client.ServerNow().UnixMilli()})
}

func localClockSnapshot(client *faroclient.Client) protocol.Snapshot {
	snapshot := client.Snapshot()
	snapshot.Playback.PositionSeconds = client.PlaybackPosition()
	snapshot.Playback.UpdatedAtUnixMs = time.Now().UnixMilli()
	return snapshot
}

func (s *Service) Disconnect() {
	s.playerLifecycleMu.Lock()
	s.mu.Lock()
	cancel, playerCancel, client, mediaPlayer := s.cancel, s.playerCancel, s.client, s.player
	streams := s.detachStreamingLocked()
	s.sessionCtx, s.cancel, s.playerCancel, s.client, s.player, s.sync = nil, nil, nil, nil, nil, nil
	s.playerDismissed = false
	s.syncState = ""
	s.mu.Unlock()
	s.playerLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if playerCancel != nil {
		playerCancel()
	}
	if client != nil {
		_ = client.Close()
	}
	if mediaPlayer != nil {
		_ = mediaPlayer.Close()
	}
	streams.close()
}

func (s *Service) Shutdown() {
	s.LeaveRoom()
}

func (s *Service) LeaveRoom() {
	s.Disconnect()
	s.StopServer()
	// Invites describe the room just left; copying one afterwards would hand
	// out a stale (or, with the owner token, sensitive) link.
	s.mu.Lock()
	s.invite, s.ownerToken = invite.Invite{}, ""
	s.mu.Unlock()
}

// reconnectDelay grows by a second per attempt up to 15 s, with jitter so a
// room whose server restarted is not hit by every client at once.
func reconnectDelay(attempt int) time.Duration {
	base := time.Duration(min(attempt, 15)) * time.Second
	return time.Duration(float64(base) * (0.8 + 0.4*rand.Float64()))
}

// permanentConnectFailure explains a reconnect failure that retrying cannot
// fix, such as a host restarting Easy hosting (new join token and address).
func permanentConnectFailure(err error) (string, bool) {
	if errors.Is(err, faroclient.ErrFingerprintMismatch) {
		return "The server's identity changed since this invite was made. Ask for a new invite to rejoin.", true
	}
	var rejected *faroclient.RejectedError
	if !errors.As(err, &rejected) || !rejected.Permanent() {
		return "", false
	}
	switch rejected.Code {
	case "unauthorized":
		return "This invite no longer works; the host may have restarted hosting. Ask for a new invite to rejoin.", true
	case "invalid_owner_token":
		return "This room now has a different owner, so the owner invite no longer works. Rejoin with a regular invite.", true
	}
	return "This version of Faro cannot rejoin the room: " + rejected.Message, true
}

// changesRoomState reports whether a server message can change the room
// snapshot. Chat, activity, errors and directed stream messages carry their
// own events; sending the whole snapshot for them only re-renders the UI.
func changesRoomState(messageType protocol.MessageType) bool {
	switch messageType {
	case protocol.TypeChatMessage, protocol.TypeActivityMessage, protocol.TypeError,
		protocol.TypeStreamRequested, protocol.TypeStreamGranted, protocol.TypeStreamRevoked:
		return false
	}
	return true
}

func syncStatusOf(status syncer.Status) SyncStatus {
	switch {
	case status.MeasuredAt.IsZero():
		return SyncStatus{State: "idle"}
	case status.Buffering:
		return SyncStatus{State: "buffering"}
	case status.InSync():
		return SyncStatus{State: "synced", DriftSeconds: status.DriftSeconds}
	}
	return SyncStatus{State: "catching-up", DriftSeconds: status.DriftSeconds}
}

// reportSync tells the page when the sync state changes; drift alone
// changing within a state is not worth a message every second.
func (s *Service) reportSync(status SyncStatus) {
	s.mu.Lock()
	changed := s.syncState != status.State
	s.syncState = status.State
	s.mu.Unlock()
	if changed {
		s.sink(Event{Kind: "sync", Sync: &status})
	}
}
