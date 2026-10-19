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

// offerStreamForItem publishes the resolved local copy for a shared-playlist item.
// Product entry points must resolve the path through OfferPlaylistStream so an
// arbitrary file can never be offered outside the queue.
func (s *Service) offerStreamForItem(path string, maxViewers int, itemID string) error {
	s.streamOfferMu.Lock()
	defer s.streamOfferMu.Unlock()
	return s.offerStreamLocked(path, maxViewers, itemID, false)
}

// offerStreamLocked publishes the offer. The caller holds streamOfferMu, so an
// automatic and an explicit share can never interleave; auto marks the offer
// as automatic in the same step that records it.
func (s *Service) offerStreamLocked(path string, maxViewers int, itemID string, auto bool) error {
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
	existing, staleOfferID := s.streamPublisher, s.streamOfferID
	ctx := s.sessionCtx
	current := s.client == client && ctx != nil
	snapshot := client.Snapshot()
	s.mu.RUnlock()
	if !current {
		return errors.New("connection changed while preparing stream offer")
	}
	if existing != nil {
		return errors.New("stop the current stream offer before offering another file")
	}
	if staleOfferID != "" {
		if err := s.stopOfferingStream(client); err != nil {
			return fmt.Errorf("finish withdrawing the previous stream offer: %w", err)
		}
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
	// Starting the transport can take a while; stopping the offer or
	// leaving the room cancels it rather than waiting for it.
	startCtx, cancel := context.WithCancel(ctx)
	start := &cancelHandle{cancel: cancel}
	s.mu.Lock()
	s.offerStart = start
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.offerStart == start {
			s.offerStart = nil
		}
		s.mu.Unlock()
		cancel()
	}()
	var publisher *mediastream.HostedPublisher
	fingerprint := identity.Media.Fingerprint
	publisher, err = mediastream.StartHostedPublisher(startCtx, s.streamFactory, mediastream.HostedPublisherConfig{
		Source: mediastream.SourceConfig{
			Path: identity.Path, Media: identity.Media,
			OnChanged: func() { s.sharedFileChanged(client, publisher, fingerprint) },
		},
		DERPMapURL: policy.DERPMapURL,
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
	s.mu.Lock()
	if s.streamPublisher == publisher {
		s.streamOfferItemID = itemID
		s.streamOfferAuto = auto
	}
	s.mu.Unlock()
	return nil
}

// cancelHandle identifies one cancellable operation, so that only its owner
// clears it.
type cancelHandle struct{ cancel context.CancelFunc }

func (s *Service) StopOfferingStream() error {
	s.mu.Lock()
	if s.offerStart != nil {
		s.offerStart.cancel()
	}
	s.mu.Unlock()
	s.streamOfferMu.Lock()
	defer s.streamOfferMu.Unlock()
	client, err := s.connected()
	if err != nil {
		return err
	}
	// Stopping explicitly means "not this one": automatic sharing leaves the
	// item alone until the selection moves on.
	s.mu.Lock()
	s.autoOfferSuppressedItem = s.streamOfferItemID
	s.mu.Unlock()
	return s.stopOfferingStream(client)
}

// stopOfferingStream releases the publisher even when the server reply is
// uncertain. Its offer ID is retained so the next attempt can finish cleanup.
// The caller holds streamOfferMu.
func (s *Service) stopOfferingStream(client *faroclient.Client) error {
	s.mu.RLock()
	publisher, offerID := s.streamPublisher, s.streamOfferID
	s.mu.RUnlock()
	if publisher == nil && offerID == "" {
		return errors.New("no media stream is being offered")
	}
	var withdrawErr error
	if offerID != "" {
		withdrawErr = client.WithdrawStreamOffer(offerID)
		var commandErr *faroclient.CommandError
		if errors.As(withdrawErr, &commandErr) && commandErr.Code == "stream_offer_not_found" {
			withdrawErr = nil
		}
	}
	s.mu.Lock()
	if s.client == client && s.streamPublisher == publisher && s.streamOfferID == offerID {
		s.streamPublisher = nil
		clear(s.streamCapabilities)
		if withdrawErr == nil {
			s.streamOfferID, s.streamOfferMedia, s.streamOfferItemID = "", nil, ""
			s.streamOfferAuto = false
		}
	}
	s.mu.Unlock()
	if publisher != nil {
		closeErr := publisher.Close()
		if withdrawErr == nil {
			return closeErr
		}
	}
	return withdrawErr
}

// sharedFileChanged withdraws an offer whose file was modified in place: its
// bytes no longer match what the room was told. The changed file is no
// longer a copy of that media either.
func (s *Service) sharedFileChanged(client *faroclient.Client, publisher *mediastream.HostedPublisher, fingerprint string) {
	s.forgetSource(fingerprint)
	s.streamOfferMu.Lock()
	defer s.streamOfferMu.Unlock()
	s.mu.RLock()
	current := publisher != nil && s.streamPublisher == publisher && s.client == client
	s.mu.RUnlock()
	if !current {
		return
	}
	s.log("offer: the shared file changed; withdrawing it")
	_ = s.stopOfferingStream(client)
	s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_source_changed", Message: "The shared file changed on this device, so sharing it stopped."}})
}

func (s *Service) offeredItemMissing(items []protocol.PlaylistItem) bool {
	s.mu.RLock()
	offerID, itemID, media := s.streamOfferID, s.streamOfferItemID, cloneStreamMedia(s.streamOfferMedia)
	s.mu.RUnlock()
	if offerID == "" || itemID == "" || media == nil {
		return false
	}
	for _, item := range items {
		if item.ID == itemID && item.Media != nil && item.Media.Fingerprint == media.Fingerprint {
			return false
		}
	}
	return true
}

func (s *Service) withdrawMissingOffer(client *faroclient.Client, items []protocol.PlaylistItem) error {
	s.streamOfferMu.Lock()
	defer s.streamOfferMu.Unlock()
	if !s.offeredItemMissing(items) {
		return nil
	}
	return s.stopOfferingStream(client)
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

// handleStreamRevoked ends whatever a revocation names: a grant this
// participant issued as a provider, or the request or grant of the stream
// it receives.
func (s *Service) handleStreamRevoked(client *faroclient.Client, revoked protocol.MediaStreamRevoked) {
	s.mu.Lock()
	capability := s.streamCapabilities[revoked.RequestID]
	publisher := s.streamPublisher
	delete(s.streamCapabilities, revoked.RequestID)
	s.mu.Unlock()
	if capability != "" && publisher != nil {
		publisher.Revoke(capability)
	}
	s.handleViewerRevocation(client, revoked)
}

// detachStreamingLocked clears session-bound transfer state when the session
// ends. The caller holds s.mu and calls the returned function after
// releasing it to close the resources.
func (s *Service) detachStreamingLocked() func() {
	if s.offerStart != nil {
		s.offerStart.cancel()
		s.offerStart = nil
	}
	publisher := s.streamPublisher
	s.streamPublisher = nil
	s.streamOfferID, s.streamOfferItemID, s.streamOfferMedia = "", "", nil
	s.streamOfferAuto = false
	if s.autoOfferTimer != nil {
		s.autoOfferTimer.Stop()
		s.autoOfferTimer, s.autoOfferKey = nil, ""
	}
	s.streamCapabilities = make(map[string]string)
	closeStream := s.detachStreamLocked()
	return func() {
		if publisher != nil {
			publisher.Close()
		}
		closeStream()
	}
}

func cloneStreamMedia(media *protocol.Media) *protocol.Media {
	if media == nil {
		return nil
	}
	copy := *media
	return &copy
}
