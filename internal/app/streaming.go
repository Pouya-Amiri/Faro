package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/mediaid"
	"github.com/Pouya-Amiri/Faro/internal/mediastream"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

type pendingStream struct {
	viewer         *mediastream.PreparedViewer
	media          protocol.Media
	requestID      string
	playlistItemID string
}

// StreamingAvailable reports whether the connected server negotiated the
// extension and supplied the operator-controlled Tailcat bootstrap policy.
func (s *Service) StreamingAvailable() bool {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil || !client.Supports(protocol.CapabilityMediaStreamV1) {
		return false
	}
	return client.Welcome().Streaming != nil
}

// offerStream publishes the resolved local copy for a shared-playlist item.
// Product entry points must resolve the path through OfferPlaylistStream so an
// arbitrary file can never be offered outside the queue.
func (s *Service) offerStream(path string, maxViewers int) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	if !client.Supports(protocol.CapabilityMediaStreamV1) || client.Welcome().Streaming == nil {
		return errors.New("this server does not support media streaming")
	}
	policy := client.Welcome().Streaming
	identity, err := mediaid.Inspect(path, "", 0)
	if err != nil {
		return err
	}
	if identity.Path == "" || !strings.HasPrefix(identity.Media.Fingerprint, "file-v1:") {
		return errors.New("only a selected regular local file can be offered")
	}
	s.mu.RLock()
	existing := s.streamPublisher
	ctx := s.sessionCtx
	snapshot := client.Snapshot()
	s.mu.RUnlock()
	if existing != nil {
		return errors.New("stop the current stream offer before offering another file")
	}
	for _, participant := range snapshot.Participants {
		if participant.ID == snapshot.SelfID && participant.Media != nil && participant.Media.Fingerprint == identity.Media.Fingerprint {
			identity.Media.DurationSeconds = participant.Media.DurationSeconds
			if participant.Media.Title != "" {
				identity.Media.Title = participant.Media.Title
			}
			break
		}
	}
	publisher, err := mediastream.StartHostedPublisher(ctx, s.streamFactory, mediastream.HostedPublisherConfig{
		Source: mediastream.SourceConfig{Path: identity.Path, Media: identity.Media}, DERPMapURL: policy.DERPMapURL,
	})
	if err != nil {
		return fmt.Errorf("start media stream publisher: %w", err)
	}
	offerID, err := secureToken()
	if err != nil {
		publisher.Close()
		return err
	}
	media := identity.Media
	s.mu.Lock()
	if s.client != client || s.streamPublisher != nil {
		s.mu.Unlock()
		publisher.Close()
		return errors.New("connection changed while starting stream offer")
	}
	s.streamPublisher, s.streamOfferID, s.streamOfferMedia = publisher, offerID, &media
	s.mu.Unlock()
	if err := client.PublishStreamOffer(protocol.MediaStreamOfferPublish{OfferID: offerID, Media: media, MaxViewers: maxViewers}); err != nil {
		s.mu.Lock()
		if s.streamPublisher == publisher {
			s.streamPublisher, s.streamOfferID, s.streamOfferMedia = nil, "", nil
		}
		s.mu.Unlock()
		publisher.Close()
		return err
	}
	return nil
}

func (s *Service) StopOfferingStream() error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	s.mu.RLock()
	publisher, offerID := s.streamPublisher, s.streamOfferID
	s.mu.RUnlock()
	if publisher == nil || offerID == "" {
		return errors.New("no media stream is being offered")
	}
	if err := client.WithdrawStreamOffer(offerID); err != nil {
		return err
	}
	s.mu.Lock()
	if s.streamPublisher == publisher {
		s.streamPublisher, s.streamOfferID, s.streamOfferMedia = nil, "", nil
		s.streamOfferItemID = ""
		clear(s.streamCapabilities)
	}
	s.mu.Unlock()
	return publisher.Close()
}

// StreamFromOffer prepares an ephemeral Tailcat identity before requesting the
// provider's connection blob. A known local copy always wins.
func (s *Service) StreamFromOffer(offerID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	snapshot := client.Snapshot()
	var offer *protocol.MediaStreamOffer
	for index := range snapshot.StreamOffers {
		candidate := snapshot.StreamOffers[index]
		if candidate.ID == offerID {
			offer = &candidate
			break
		}
	}
	if offer == nil {
		return errors.New("media stream offer is no longer available")
	}
	playlistItemID := ""
	if selected := snapshot.Playlist.Selected; selected >= 0 && selected < len(snapshot.Playlist.Items) {
		item := snapshot.Playlist.Items[selected]
		if item.Media != nil && item.Media.Fingerprint == offer.Media.Fingerprint {
			playlistItemID = item.ID
		}
	}
	s.mu.Lock()
	if s.sources[offer.Media.Fingerprint] != "" {
		s.mu.Unlock()
		return errors.New("a matching local copy is already available and remains preferred")
	}
	if s.pendingStreams[offerID] != nil {
		s.mu.Unlock()
		return errors.New("this media stream is already being requested")
	}
	factory := s.streamFactory
	s.mu.Unlock()
	viewer, err := mediastream.PrepareViewer(factory)
	if err != nil {
		return err
	}
	pending := &pendingStream{viewer: viewer, media: offer.Media, playlistItemID: playlistItemID}
	s.mu.Lock()
	s.pendingStreams[offerID] = pending
	s.mu.Unlock()
	accepted, err := client.RequestStream(protocol.MediaStreamRequest{OfferID: offerID, ClientPublicKey: viewer.PublicKey()})
	if err != nil {
		s.mu.Lock()
		if s.pendingStreams[offerID] == pending {
			delete(s.pendingStreams, offerID)
		}
		s.mu.Unlock()
		viewer.Close()
		return err
	}
	s.mu.Lock()
	if s.pendingStreams[offerID] == pending {
		pending.requestID = accepted.RequestID
		s.pendingRequestOffers[accepted.RequestID] = offerID
	}
	s.mu.Unlock()
	s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "requesting", OfferID: offerID}})
	return nil
}

// reconcilePlaylistStreams makes shared playback automatic: when the selected
// queue item has no local copy, the first available room offer is accepted.
// It also tears down transfers that no longer belong to the selected queue
// item, including a file removed from the queue while it is being streamed.
func (s *Service) reconcilePlaylistStreams(ctx context.Context, client *faroclient.Client) {
	if ctx.Err() != nil {
		return
	}
	snapshot := client.Snapshot()
	allMedia := make(map[string]bool, len(snapshot.Playlist.Items))
	for _, item := range snapshot.Playlist.Items {
		if item.Media != nil && item.Media.Fingerprint != "" {
			allMedia[item.Media.Fingerprint] = true
		}
	}
	var selectedFingerprint, selectedItemID string
	if snapshot.Playlist.Selected >= 0 && snapshot.Playlist.Selected < len(snapshot.Playlist.Items) {
		selected := snapshot.Playlist.Items[snapshot.Playlist.Selected]
		selectedItemID = selected.ID
		if media := selected.Media; media != nil {
			selectedFingerprint = media.Fingerprint
		}
	}

	s.mu.RLock()
	if s.client != client {
		s.mu.RUnlock()
		return
	}
	offeredItemID := s.streamOfferItemID
	receiving := cloneStreamMedia(s.streamIdentity)
	receivingItemID := s.streamReceiveItemID
	activation := s.streamActivation
	playerDismissed := s.playerDismissed
	pendingCount := len(s.pendingStreams)
	hasPending := pendingCount != 0 || s.streamActivationCancel != nil
	pendingSelectionInvalid := false
	for _, pending := range s.pendingStreams {
		// Requests made outside playlist playback are intentionally unbound and
		// retain the explicit StreamFromOffer/StopStreaming API semantics.
		if pending.playlistItemID != "" && pending.playlistItemID != selectedItemID {
			pendingSelectionInvalid = true
		}
	}
	if activation != nil && activation.playlistItemID != "" && activation.playlistItemID != selectedItemID {
		pendingSelectionInvalid = true
	}
	localSource := s.sources[selectedFingerprint]
	s.mu.RUnlock()
	if playerDismissed {
		return
	}

	offeredItemPresent := offeredItemID == ""
	for _, item := range snapshot.Playlist.Items {
		if item.ID == offeredItemID {
			offeredItemPresent = true
			break
		}
	}
	if !offeredItemPresent {
		_ = s.StopOfferingStream()
	}
	if receiving != nil && receivingItemID != "" && (!allMedia[receiving.Fingerprint] || receivingItemID != selectedItemID) {
		_ = s.StopStreaming()
		receiving = nil
		hasPending = false
	}
	if pendingSelectionInvalid {
		_ = s.StopStreaming()
		hasPending = false
	}
	if selectedFingerprint == "" || localSource != "" || receiving != nil || hasPending {
		return
	}
	for _, offer := range snapshot.StreamOffers {
		if offer.ProviderID != snapshot.SelfID && offer.Media.Fingerprint == selectedFingerprint && offer.ViewerCount < offer.MaxViewers {
			_ = s.StreamFromOffer(offer.ID)
			return
		}
	}
}

func (s *Service) StopStreaming() error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	s.mu.Lock()
	gateway, requestID := s.streamGateway, s.streamRequestID
	activationCancel := s.streamActivationCancel
	s.streamActivationCancel = nil
	s.streamActivation = nil
	pending := make([]*pendingStream, 0, len(s.pendingStreams))
	for offerID, stream := range s.pendingStreams {
		pending = append(pending, stream)
		delete(s.pendingStreams, offerID)
		if stream.requestID != "" {
			delete(s.pendingRequestOffers, stream.requestID)
		}
	}
	s.streamGateway, s.streamRequestID, s.streamReceiveItemID, s.streamIdentity = nil, "", "", nil
	s.mu.Unlock()
	if activationCancel != nil {
		activationCancel()
	}
	if gateway == nil && len(pending) == 0 && activationCancel == nil {
		return errors.New("no media stream is active")
	}
	for _, stream := range pending {
		_ = stream.viewer.Close()
		if stream.requestID != "" {
			_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: stream.requestID, Reason: "viewer cancelled stream request"})
		}
	}
	if gateway != nil {
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: requestID, Reason: "viewer stopped streaming"})
		_ = gateway.Close()
		s.pauseUnavailableStream(client, "Streaming stopped. Locate a local copy to continue.")
	}
	s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
	return nil
}

func (s *Service) handleStreamRequest(ctx context.Context, client *faroclient.Client, request protocol.MediaStreamRequested) {
	if ctx.Err() != nil {
		return
	}
	s.mu.RLock()
	publisher, offerID, currentClient := s.streamPublisher, s.streamOfferID, s.client
	s.mu.RUnlock()
	if currentClient != client || publisher == nil || offerID != request.OfferID {
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: request.RequestID, Reason: "provider is no longer offering the file"})
		return
	}
	expiresAt := time.UnixMilli(request.ExpiresAtUnixMs)
	capability, err := publisher.Grant(request.ClientPublicKey, expiresAt)
	if err == nil {
		err = client.GrantStream(protocol.MediaStreamGrant{
			RequestID: request.RequestID, ConnectionBlob: publisher.ConnectionBlob(),
			TransferCapability: capability, ExpiresAtUnixMs: expiresAt.UnixMilli(),
		})
	}
	if err != nil {
		if capability != "" {
			publisher.Revoke(capability)
		}
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: request.RequestID, Reason: "provider could not admit the stream"})
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_grant", Message: err.Error()}})
		return
	}
	s.mu.Lock()
	stillCurrent := s.client == client && s.streamPublisher == publisher && s.streamOfferID == request.OfferID
	if stillCurrent {
		s.streamCapabilities[request.RequestID] = capability
	}
	s.mu.Unlock()
	if !stillCurrent {
		publisher.Revoke(capability)
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: request.RequestID, Reason: "provider stopped offering while admitting the stream"})
	}
}

func (s *Service) activateStream(ctx context.Context, client *faroclient.Client, grant protocol.MediaStreamGranted) {
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	pending := s.pendingStreams[grant.OfferID]
	if pending == nil || pending.requestID != "" && pending.requestID != grant.RequestID {
		s.mu.Unlock()
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grant.RequestID, Reason: "viewer no longer wants the stream"})
		return
	}
	delete(s.pendingStreams, grant.OfferID)
	delete(s.pendingRequestOffers, grant.RequestID)
	activationCtx, activationCancel := context.WithCancel(ctx)
	s.streamActivationCancel = activationCancel
	s.streamActivation = pending
	s.mu.Unlock()
	ctx = activationCtx
	defer func() {
		activationCancel()
		s.mu.Lock()
		if s.streamActivation == pending {
			s.streamActivationCancel = nil
			s.streamActivation = nil
		}
		s.mu.Unlock()
	}()
	gateway, err := pending.viewer.StartGateway(ctx, grant, pending.media)
	if err != nil {
		s.failStreamActivation(client, grant, pending, nil, "stream_connect", err)
		return
	}
	currentClient, mediaPlayer, err := s.ensurePlayer(playerStartAutomatic)
	if err != nil || currentClient != client {
		if err == nil {
			err = errors.New("connection changed while starting stream")
		}
		if errors.Is(err, errPlayerDismissed) {
			if gateway != nil {
				_ = gateway.Close()
			} else {
				_ = pending.viewer.Close()
			}
			_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grant.RequestID, Reason: "viewer closed the media player"})
			s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
			return
		}
		s.failStreamActivation(client, grant, pending, gateway, "stream_player", err)
		return
	}
	openCtx, cancel := s.mediaOperationContext(ctx)
	defer cancel()
	s.beginMediaTransition()
	finished := false
	defer func() {
		if !finished {
			s.endMediaTransition()
		}
	}()
	if _, err := s.openPlayerSource(openCtx, mediaPlayer, gateway.URL()); err != nil {
		s.failStreamActivation(client, grant, pending, gateway, "stream_open", err)
		return
	}
	s.waitForPlayerMedia(openCtx, mediaPlayer, pending.media.DurationSeconds)
	playback := localClockSnapshot(client).Playback
	_ = mediaPlayer.Seek(openCtx, playback.PositionSeconds)
	_ = mediaPlayer.SetRate(openCtx, normalizedRate(playback.Rate))
	if err := client.SetMedia(protocol.MediaSet{Media: &pending.media}); err != nil {
		s.failStreamActivation(client, grant, pending, gateway, "stream_identity", err)
		return
	}
	s.mu.Lock()
	if ctx.Err() != nil || s.client != client || s.player != mediaPlayer {
		s.mu.Unlock()
		s.failStreamActivation(client, grant, pending, gateway, "stream_cancelled", errors.New("stream was cancelled while loading"))
		return
	}
	s.streamActivationCancel = nil
	s.streamActivation = nil
	previous := s.streamGateway
	identity := pending.media
	s.streamGateway, s.streamRequestID, s.streamReceiveItemID, s.streamIdentity = gateway, grant.RequestID, pending.playlistItemID, &identity
	playlist := client.Snapshot().Playlist
	if playlist.Selected >= 0 && playlist.Selected < len(playlist.Items) {
		item := playlist.Items[playlist.Selected]
		if item.Media != nil && item.Media.Fingerprint == identity.Fingerprint {
			s.selectedItem = item.ID
		}
	}
	s.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "active", OfferID: grant.OfferID, Route: "direct"}})
	if _, err := s.finishMediaTransition(openCtx, mediaPlayer, localClockSnapshot(client).Playback.Paused, nil); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_sync", Message: err.Error()}})
	}
	finished = true
}

func (s *Service) failStreamActivation(client *faroclient.Client, grant protocol.MediaStreamGranted, pending *pendingStream, gateway *mediastream.Gateway, code string, cause error) {
	if gateway != nil {
		_ = gateway.Close()
	} else if pending != nil {
		_ = pending.viewer.Close()
	}
	_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grant.RequestID, Reason: "viewer could not activate the stream"})
	s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
	s.sink(Event{Kind: "error", Error: &protocol.Error{Code: code, Message: cause.Error()}})
}

func (s *Service) handleStreamRevoked(client *faroclient.Client, revoked protocol.MediaStreamRevoked) {
	s.mu.Lock()
	pendingClosed := false
	var capability string
	var publisher *mediastream.HostedPublisher
	var pendingViewer *mediastream.PreparedViewer
	capability = s.streamCapabilities[revoked.RequestID]
	if capability != "" {
		publisher = s.streamPublisher
		delete(s.streamCapabilities, revoked.RequestID)
	}
	if offerID := s.pendingRequestOffers[revoked.RequestID]; offerID != "" {
		if pending := s.pendingStreams[offerID]; pending != nil {
			pendingViewer = pending.viewer
			delete(s.pendingStreams, offerID)
			pendingClosed = true
		}
		delete(s.pendingRequestOffers, revoked.RequestID)
	}
	var gateway *mediastream.Gateway
	if s.streamRequestID == revoked.RequestID {
		gateway = s.streamGateway
		s.streamGateway, s.streamRequestID, s.streamReceiveItemID, s.streamIdentity = nil, "", "", nil
	}
	s.mu.Unlock()
	if publisher != nil && capability != "" {
		publisher.Revoke(capability)
	}
	if pendingViewer != nil {
		pendingViewer.Close()
	}
	if pendingClosed {
		s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
	}
	if gateway != nil {
		gateway.Close()
		s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
		s.pauseUnavailableStream(client, "The media provider stopped streaming. Locate a local copy or choose another provider.")
	}
}

func (s *Service) pauseUnavailableStream(client *faroclient.Client, message string) {
	s.mu.RLock()
	mediaPlayer := s.player
	s.mu.RUnlock()
	if mediaPlayer != nil {
		ctx, cancel := context.WithTimeout(s.root, 3*time.Second)
		_ = mediaPlayer.SetPaused(ctx, true)
		cancel()
	}
	_ = client.SetMedia(protocol.MediaSet{Media: nil})
	s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_unavailable", Message: message}})
}

type streamingResources struct {
	publisher *mediastream.HostedPublisher
	gateway   *mediastream.Gateway
	viewers   []*mediastream.PreparedViewer
}

// detachStreamingLocked clears session-bound transfer state. The caller must
// hold s.mu and close the returned resources after releasing it.
func (s *Service) detachStreamingLocked() streamingResources {
	if s.streamActivationCancel != nil {
		s.streamActivationCancel()
		s.streamActivationCancel = nil
	}
	s.streamActivation = nil
	resources := streamingResources{publisher: s.streamPublisher, gateway: s.streamGateway}
	for _, pending := range s.pendingStreams {
		resources.viewers = append(resources.viewers, pending.viewer)
	}
	s.streamPublisher, s.streamGateway = nil, nil
	s.streamOfferID, s.streamOfferItemID, s.streamRequestID, s.streamReceiveItemID = "", "", "", ""
	s.streamOfferMedia, s.streamIdentity = nil, nil
	s.streamCapabilities = make(map[string]string)
	s.pendingStreams = make(map[string]*pendingStream)
	s.pendingRequestOffers = make(map[string]string)
	if resources.gateway != nil {
		s.currentSource = ""
	}
	return resources
}

func (r streamingResources) close() {
	if r.publisher != nil {
		r.publisher.Close()
	}
	if r.gateway != nil {
		r.gateway.Close()
	}
	for _, viewer := range r.viewers {
		viewer.Close()
	}
}

func cloneStreamMedia(media *protocol.Media) *protocol.Media {
	if media == nil {
		return nil
	}
	copy := *media
	return &copy
}
