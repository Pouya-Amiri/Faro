package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/mediastream"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

// streamResumeTimeout is how long playback continues from the cache after a
// provider's grant ended (it was withdrawn, expired or refused) while a new
// grant is requested. If none arrives the stream ends as unavailable.
var streamResumeTimeout = 30 * time.Second

// streamRetryDelay is how long a provider whose stream could not be set up
// is passed over by automatic requests; it doubles with each failure up to
// maxStreamRetryDelay.
var (
	streamRetryDelay    = 3 * time.Second
	maxStreamRetryDelay = time.Minute
)

// streamPhase is where a viewer stream is in its lifecycle.
type streamPhase uint8

const (
	// streamRequesting: a grant was requested and has not arrived.
	streamRequesting streamPhase = iota
	// streamActivating: the grant arrived; the gateway connects and the
	// player opens it.
	streamActivating
	// streamActive: the player plays from the gateway.
	streamActive
	// streamResuming: the grant ended while the file was not yet fully
	// cached. The player keeps playing from the cache while a new grant is
	// requested, and the transfer continues where it was.
	streamResuming
)

// viewerStream is one file this participant receives, from the first
// request until it is retired. Only the stream that is Service.stream may
// change shared state; a retired one merely releases its own resources, so
// a late acknowledgement, grant, failure or cleanup of an old stream can
// never affect a newer one.
type viewerStream struct {
	id     uint64
	itemID string // the queue item it plays; "" when requested outside the queue
	ctx    context.Context
	cancel context.CancelFunc

	// Guarded by Service.mu.
	media   protocol.Media
	phase   streamPhase
	attempt *streamAttempt // the request in flight, if any
	grantID string         // the request whose grant feeds the gateway
	offerID string         // the offer of that grant
	gateway *mediastream.Gateway
	cached  bool // every byte is cached; no provider is needed any more
	resume  *time.Timer
}

// streamAttempt is one request for a grant, with the transport identity it
// was made with.
type streamAttempt struct {
	viewer     *mediastream.PreparedViewer
	offerID    string
	providerID string
	requestID  string // set by the acknowledgement, or by a grant that arrived first
}

// streamFailure delays automatic requests to a provider whose stream could
// not be set up, so another provider is tried and a broken one is not
// hammered.
type streamFailure struct {
	count int
	until time.Time
}

var errLocalCopyPreferred = errors.New("a matching local copy is already available and remains preferred")

// StreamFromOffer requests the offered file. A known local copy always wins.
func (s *Service) StreamFromOffer(offerID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	snapshot := client.Snapshot()
	for _, offer := range snapshot.StreamOffers {
		if offer.ID != offerID {
			continue
		}
		itemID := ""
		if item, ok := selectedPlaylistItem(snapshot.Playlist); ok && item.Media != nil && item.Media.Fingerprint == offer.Media.Fingerprint {
			itemID = item.ID
		}
		return s.requestStream(client, offer, itemID)
	}
	return errors.New("media stream offer is no longer available")
}

// requestStream starts a new stream of offer for the queue item itemID.
func (s *Service) requestStream(client *faroclient.Client, offer protocol.MediaStreamOffer, itemID string) error {
	if s.localSource(offer.Media.Fingerprint) != "" {
		return errLocalCopyPreferred
	}
	s.mu.RLock()
	factory, busy := s.streamFactory, s.stream != nil
	s.mu.RUnlock()
	if busy {
		return errors.New("a media stream is already being received")
	}
	viewer, err := mediastream.PrepareViewer(factory)
	if err != nil {
		return err
	}
	attempt := &streamAttempt{viewer: viewer, offerID: offer.ID, providerID: offer.ProviderID}
	s.mu.Lock()
	if s.client != client || s.sessionCtx == nil {
		s.mu.Unlock()
		viewer.Close()
		return errors.New("connection changed while requesting the stream")
	}
	if s.stream != nil {
		s.mu.Unlock()
		viewer.Close()
		return errors.New("a media stream is already being received")
	}
	s.streamSeq++
	ctx, cancel := context.WithCancel(s.sessionCtx)
	stream := &viewerStream{
		id: s.streamSeq, itemID: itemID, ctx: ctx, cancel: cancel,
		media: offer.Media, phase: streamRequesting, attempt: attempt,
	}
	s.stream = stream
	s.mu.Unlock()
	s.log("stream %d: requesting %s from a provider", stream.id, mediaLabel(offer.Media))
	s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "requesting", OfferID: offer.ID}})
	return s.sendStreamRequest(client, stream, attempt)
}

// sendStreamRequest asks the server for a grant on stream's behalf.
func (s *Service) sendStreamRequest(client *faroclient.Client, stream *viewerStream, attempt *streamAttempt) error {
	accepted, err := client.RequestStream(protocol.MediaStreamRequest{OfferID: attempt.offerID, ClientPublicKey: attempt.viewer.PublicKey()})
	s.mu.Lock()
	inFlight := s.stream == stream && stream.attempt == attempt
	if err == nil && inFlight && attempt.requestID == "" {
		attempt.requestID = accepted.RequestID
	}
	// A grant may have arrived before the acknowledgement; then the request
	// is already the stream's grant.
	wanted := inFlight || err == nil && s.stream == stream && stream.grantID == accepted.RequestID
	s.mu.Unlock()
	switch {
	case err != nil && inFlight:
		s.streamAttemptFailed(client, stream, attempt, err)
		return err
	case err != nil:
		return err
	case !wanted:
		// Cancelled before the acknowledgement named the request: withdraw
		// it now, or it would reserve the offer until it expires.
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: accepted.RequestID, Reason: "viewer cancelled stream request"})
		return errors.New("the stream request was cancelled")
	}
	return nil
}

// streamAttemptFailed handles a request that was refused, expired or could
// not be set up. A stream that was never playing ends; one that is resuming
// keeps playing from its cache and tries again, with another provider if
// there is one.
func (s *Service) streamAttemptFailed(client *faroclient.Client, stream *viewerStream, attempt *streamAttempt, cause error) {
	s.mu.Lock()
	if s.stream != stream || stream.attempt != attempt {
		s.mu.Unlock()
		attempt.viewer.Close()
		return
	}
	stream.attempt = nil
	s.noteStreamFailureLocked(attempt.providerID, stream.media.Fingerprint)
	resuming := stream.phase == streamResuming
	s.mu.Unlock()
	attempt.viewer.Close()
	s.log("stream %d: request failed: %v", stream.id, cause)
	if !resuming {
		s.retireStream(client, stream, nil)
	}
	s.reconcileSoon(client)
}

// reconcileSoon reconciles streams again in the background, so a failed
// stream is retried with another provider, or once its provider's cooldown
// has passed.
func (s *Service) reconcileSoon(client *faroclient.Client) {
	s.mu.RLock()
	ctx, current := s.sessionCtx, s.client == client
	s.mu.RUnlock()
	if current && ctx != nil {
		go s.reconcilePlaylistStreams(ctx, client)
	}
}

// activateStream handles a grant. The first grant of a stream connects a
// new gateway and opens it in the player; a grant for a resuming stream
// reconnects its existing gateway.
func (s *Service) activateStream(ctx context.Context, client *faroclient.Client, grant protocol.MediaStreamGranted) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	stream := s.stream
	var attempt *streamAttempt
	if stream != nil {
		attempt = stream.attempt
	}
	if s.client != client || attempt == nil || attempt.offerID != grant.OfferID || attempt.requestID != "" && attempt.requestID != grant.RequestID {
		s.mu.Unlock()
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grant.RequestID, Reason: "viewer no longer wants the stream"})
		return
	}
	attempt.requestID = grant.RequestID
	stream.attempt, stream.grantID, stream.offerID = nil, grant.RequestID, grant.OfferID
	resuming := stream.phase == streamResuming
	if !resuming {
		stream.phase = streamActivating
	}
	s.mu.Unlock()
	if resuming {
		s.resumeStream(client, stream, attempt, grant)
		return
	}
	s.startStream(client, stream, attempt, grant)
}

func (s *Service) startStream(client *faroclient.Client, stream *viewerStream, attempt *streamAttempt, grant protocol.MediaStreamGranted) {
	s.mu.RLock()
	cache, media := s.streamCache, stream.media
	s.mu.RUnlock()
	gateway, err := attempt.viewer.StartGateway(stream.ctx, grant, media, mediastream.GatewayOptions{
		Cache:             cache,
		OnUpstreamRefused: func(err error) { s.upstreamLost(client, stream, grant.RequestID, true, err) },
		Logf:              s.streamLogf(stream),
	})
	if err != nil {
		s.mu.Lock()
		if s.stream == stream {
			s.noteStreamFailureLocked(attempt.providerID, media.Fingerprint)
		}
		s.mu.Unlock()
		if s.retireStream(client, stream, &protocol.Error{Code: "stream_connect", Message: err.Error()}) {
			s.reconcileSoon(client)
		}
		return
	}
	s.mu.Lock()
	if s.stream != stream {
		s.mu.Unlock()
		gateway.Close()
		return
	}
	stream.gateway = gateway
	s.mu.Unlock()
	s.log("stream %d: connected to the provider", stream.id)
	s.playStream(client, stream, gateway)
}

// playStream opens a newly connected stream in the player and commits it as
// the player's media.
func (s *Service) playStream(client *faroclient.Client, stream *viewerStream, gateway *mediastream.Gateway) {
	_, mediaPlayer, err := s.ensurePlayer(playerStartAutomatic)
	if err != nil {
		var failure *protocol.Error
		if !errors.Is(err, errPlayerDismissed) {
			failure = &protocol.Error{Code: "stream_player", Message: err.Error()}
		}
		s.retireStream(client, stream, failure)
		return
	}
	load := s.beginLoad(stream.ctx, fmt.Sprintf("stream:%d", stream.id))
	if load == nil {
		return
	}
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	defer s.endLoad(load)
	result, err := s.loadSource(load, client, mediaPlayer, gateway.URL())
	if err != nil {
		var failure *protocol.Error
		if stream.ctx.Err() == nil && load.ctx.Err() == nil {
			failure = &protocol.Error{Code: "stream_open", Message: err.Error()}
		}
		s.retireStream(client, stream, failure)
		return
	}
	s.mu.Lock()
	if s.stream != stream || stream.phase != streamActivating || !s.loadOwnsPlayerLocked(load, client, mediaPlayer) {
		s.mu.Unlock()
		s.retireStream(client, stream, nil)
		return
	}
	// An offer made before the sharer's player probed the file carries no
	// duration; the viewer's player knows it once the stream opens.
	if stream.media.DurationSeconds <= 0 && result.state.DurationSeconds > 0 {
		stream.media.DurationSeconds = result.state.DurationSeconds
	}
	stream.phase = streamActive
	media := stream.media
	gen := s.commitSourceLocked(result)
	s.manualSource = ""
	if item, ok := selectedPlaylistItem(client.Snapshot().Playlist); ok && item.Media != nil && item.Media.Fingerprint == media.Fingerprint {
		s.selectedItem, s.selectedItemIdentity = item.ID, playlistItemIdentity(item)
	}
	s.clearStreamFailureLocked(media.Fingerprint)
	s.mu.Unlock()
	if err := s.publishMedia(client, gen, &media); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_identity", Message: err.Error()}})
	}
	s.log("stream %d: playing", stream.id)
	s.emitStreamStatus(stream)
	go s.reportStreamProgress(stream, gateway)
	if _, err := s.finishLoad(load, mediaPlayer, localClockSnapshot(client).Playback.Paused, nil); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_sync", Message: err.Error()}})
	}
}

// resumeStream continues a resuming stream's transfer through a new grant.
func (s *Service) resumeStream(client *faroclient.Client, stream *viewerStream, attempt *streamAttempt, grant protocol.MediaStreamGranted) {
	s.mu.RLock()
	gateway := stream.gateway
	s.mu.RUnlock()
	if err := attempt.viewer.Resume(stream.ctx, grant, gateway); err != nil {
		s.mu.Lock()
		current := s.stream == stream && stream.grantID == grant.RequestID
		if current {
			stream.grantID = ""
			s.noteStreamFailureLocked(attempt.providerID, stream.media.Fingerprint)
		}
		s.mu.Unlock()
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grant.RequestID, Reason: "viewer could not resume the stream"})
		if current {
			s.log("stream %d: resuming failed: %v", stream.id, err)
			s.reconcileSoon(client)
		}
		return
	}
	s.mu.Lock()
	if s.stream != stream || stream.grantID != grant.RequestID || stream.phase != streamResuming {
		s.mu.Unlock()
		return
	}
	stream.phase = streamActive
	if stream.resume != nil {
		stream.resume.Stop()
		stream.resume = nil
	}
	s.clearStreamFailureLocked(stream.media.Fingerprint)
	s.mu.Unlock()
	s.log("stream %d: resumed with a new grant", stream.id)
	s.emitStreamStatus(stream)
}

// upstreamLost handles the end of the grant feeding a stream: the server
// revoked it, it expired, or the provider refused the transfer (then revoke
// asks the server to release the grant too, so a new one can be requested).
// A stream still activating fails. An active one keeps playing: from the
// cache alone if the whole file is there, otherwise while a new grant is
// requested.
func (s *Service) upstreamLost(client *faroclient.Client, stream *viewerStream, grantID string, revoke bool, cause error) {
	s.mu.Lock()
	if s.stream != stream || stream.grantID != grantID {
		s.mu.Unlock()
		return
	}
	stream.grantID = ""
	phase, gateway := stream.phase, stream.gateway
	complete := gateway != nil && gateway.Complete()
	switch {
	case phase == streamActive && complete:
		stream.cached = true
	case phase == streamActive:
		stream.phase = streamResuming
		stream.resume = time.AfterFunc(streamResumeTimeout, func() { s.resumeExpired(client, stream) })
	}
	s.mu.Unlock()
	if revoke {
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grantID, Reason: "viewer lost access to the stream"})
	}
	if phase != streamActive {
		s.retireStream(client, stream, &protocol.Error{Code: "stream_connect", Message: cause.Error()})
		return
	}
	gateway.Disconnect()
	if complete {
		s.log("stream %d: grant ended (%v); playing from the complete cache", stream.id, cause)
	} else {
		s.log("stream %d: grant ended (%v); requesting a new one", stream.id, cause)
		s.reconcileSoon(client)
	}
	s.emitStreamStatus(stream)
}

func (s *Service) resumeExpired(client *faroclient.Client, stream *viewerStream) {
	s.mu.RLock()
	expired := s.stream == stream && stream.phase == streamResuming
	s.mu.RUnlock()
	if expired {
		s.log("stream %d: no provider resumed the stream", stream.id)
		s.retireStream(client, stream, &protocol.Error{
			Code: "stream_unavailable", Message: "The media provider stopped streaming. Locate a local copy or choose another provider.",
		})
	}
}

// retireStream ends stream if it is still the current stream and reports
// whether it was. It releases the stream's transport, withdraws its request
// or grant, and closes its gateway. If the player shows the stream, the
// player is paused and the room told it has no media, but only while
// nothing newer owns the player. Events are emitted only for the stream
// that was current; failure, if any, is reported to the user.
func (s *Service) retireStream(client *faroclient.Client, stream *viewerStream, failure *protocol.Error) bool {
	s.mu.Lock()
	if s.stream != stream {
		s.mu.Unlock()
		return false
	}
	s.stream = nil
	stream.cancel()
	attempt, grantID, gateway := stream.attempt, stream.grantID, stream.gateway
	stream.attempt, stream.grantID = nil, ""
	if stream.resume != nil {
		stream.resume.Stop()
	}
	var mediaPlayer player.Player
	var gen uint64
	showing := gateway != nil && s.currentSource == gateway.URL()
	if showing {
		gen = s.invalidateMediaLocked()
		mediaPlayer = s.player
	}
	s.mu.Unlock()
	if attempt != nil {
		attempt.viewer.Close()
		if attempt.requestID != "" {
			_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: attempt.requestID, Reason: "viewer cancelled stream request"})
		}
	}
	if grantID != "" {
		_ = client.RevokeStream(protocol.MediaStreamRevoke{RequestID: grantID, Reason: "viewer stopped streaming"})
	}
	if gateway != nil {
		gateway.Close()
	}
	if showing {
		s.pausePlayerIfCurrent(mediaPlayer, gen)
		_ = s.publishMedia(client, gen, nil)
	}
	reason := "retired"
	if failure != nil {
		reason = failure.Message
	}
	s.log("stream %d: ended: %s", stream.id, reason)
	s.sink(Event{Kind: "stream", Stream: &StreamStatus{State: "idle"}})
	if failure != nil {
		s.sink(Event{Kind: "error", Error: failure})
	}
	return true
}

// StopStreaming ends the stream being received. Automatic streaming leaves
// the item alone until someone plays something again.
func (s *Service) StopStreaming() error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	s.mu.Lock()
	stream := s.stream
	if stream != nil {
		s.streamSuppressedItem = stream.itemID
	}
	s.mu.Unlock()
	if stream == nil || !s.retireStream(client, stream, nil) {
		return errors.New("no media stream is active")
	}
	return nil
}

// resumeStreamingIntent lifts StopStreaming's suppression when someone plays
// something.
func (s *Service) resumeStreamingIntent() {
	s.mu.Lock()
	s.streamSuppressedItem = ""
	s.mu.Unlock()
}

// reconcilePlaylistStreams makes shared playback automatic: when the selected
// queue item has no local copy, the first usable room offer is requested,
// and a stream whose grant ended is resumed through another one. It also
// retires a stream that no longer belongs to the selected item, including a
// file removed from the queue while it is being streamed.
func (s *Service) reconcilePlaylistStreams(ctx context.Context, client *faroclient.Client) {
	if ctx.Err() != nil {
		return
	}
	snapshot := client.Snapshot()
	selectedItemID, fingerprint := "", ""
	if item, ok := selectedPlaylistItem(snapshot.Playlist); ok {
		selectedItemID = item.ID
		if item.Media != nil {
			fingerprint = item.Media.Fingerprint
		}
	}
	s.mu.RLock()
	if s.client != client {
		s.mu.RUnlock()
		return
	}
	stream, dismissed, suppressed := s.stream, s.playerDismissed, s.streamSuppressedItem
	obsolete := stream != nil && stream.itemID != "" && (stream.itemID != selectedItemID || stream.media.Fingerprint != fingerprint)
	s.mu.RUnlock()

	if err := s.withdrawMissingOffer(client, snapshot.Playlist.Items); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_offer_withdraw", Message: err.Error()}})
	}
	if obsolete {
		// Retiring is scoped to the stream this decision was about: a newer
		// stream started meanwhile is left alone.
		s.retireStream(client, stream, nil)
		stream = nil
	}
	if stream != nil {
		// A stream whose grant ended continues through any offer of the same
		// file, whether or not its player is open.
		s.mu.RLock()
		resume := stream.phase == streamResuming && stream.attempt == nil
		streamFingerprint := stream.media.Fingerprint
		s.mu.RUnlock()
		if resume && s.localSource(streamFingerprint) == "" {
			if offer, ok := s.offerOrRetry(client, snapshot, streamFingerprint); ok {
				s.requestResume(client, stream, offer)
			}
		}
		return
	}
	// Closing the player suppresses automatic playback, but not the cleanup
	// above: a removed queue item must not keep its transfer alive.
	if dismissed || fingerprint == "" || suppressed != "" && suppressed == selectedItemID || s.localSource(fingerprint) != "" {
		return
	}
	if offer, ok := s.offerOrRetry(client, snapshot, fingerprint); ok {
		_ = s.requestStream(client, offer, selectedItemID)
	}
}

// offerOrRetry chooses an offer for fingerprint. When every candidate is
// cooling down after a failure, it schedules another attempt for when the
// first one may be tried.
func (s *Service) offerOrRetry(client *faroclient.Client, snapshot protocol.Snapshot, fingerprint string) (protocol.MediaStreamOffer, bool) {
	offer, wait, ok := s.chooseOffer(snapshot, fingerprint)
	if !ok && wait > 0 {
		s.scheduleStreamRetry(client, wait)
	}
	return offer, ok
}

// requestResume asks offer's provider for a new grant for a resuming stream.
func (s *Service) requestResume(client *faroclient.Client, stream *viewerStream, offer protocol.MediaStreamOffer) {
	s.mu.RLock()
	factory := s.streamFactory
	s.mu.RUnlock()
	viewer, err := mediastream.PrepareViewer(factory)
	if err != nil {
		return
	}
	attempt := &streamAttempt{viewer: viewer, offerID: offer.ID, providerID: offer.ProviderID}
	s.mu.Lock()
	if s.stream != stream || stream.phase != streamResuming || stream.attempt != nil {
		s.mu.Unlock()
		viewer.Close()
		return
	}
	stream.attempt = attempt
	s.mu.Unlock()
	s.log("stream %d: requesting a new grant", stream.id)
	_ = s.sendStreamRequest(client, stream, attempt)
}

// chooseOffer picks the offer to request for fingerprint: another
// participant's, with room for a viewer, from a provider that has not just
// failed. When every candidate is cooling down it reports how long until
// the first one may be tried.
func (s *Service) chooseOffer(snapshot protocol.Snapshot, fingerprint string) (protocol.MediaStreamOffer, time.Duration, bool) {
	now := time.Now()
	wait := time.Duration(0)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, offer := range snapshot.StreamOffers {
		if offer.ProviderID == snapshot.SelfID || offer.Media.Fingerprint != fingerprint || offer.ViewerCount >= offer.MaxViewers {
			continue
		}
		if failure := s.streamFailures[streamFailureKey(offer.ProviderID, fingerprint)]; failure != nil && now.Before(failure.until) {
			if remaining := failure.until.Sub(now); wait == 0 || remaining < wait {
				wait = remaining
			}
			continue
		}
		return offer, 0, true
	}
	return protocol.MediaStreamOffer{}, wait, false
}

// failureKey identifies one provider's copy of one file.
type failureKey struct{ provider, fingerprint string }

func streamFailureKey(providerID, fingerprint string) failureKey {
	return failureKey{provider: providerID, fingerprint: fingerprint}
}

func (s *Service) noteStreamFailureLocked(providerID, fingerprint string) {
	key := streamFailureKey(providerID, fingerprint)
	failure := s.streamFailures[key]
	if failure == nil {
		failure = &streamFailure{}
		s.streamFailures[key] = failure
	}
	failure.count++
	failure.until = time.Now().Add(min(streamRetryDelay<<min(failure.count-1, 8), maxStreamRetryDelay))
}

func (s *Service) clearStreamFailureLocked(fingerprint string) {
	for key := range s.streamFailures {
		if key.fingerprint == fingerprint {
			delete(s.streamFailures, key)
		}
	}
}

// scheduleStreamRetry reconciles again once a provider's cooldown has
// passed, so automatic streaming recovers without waiting for a room event.
func (s *Service) scheduleStreamRetry(client *faroclient.Client, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != client || s.sessionCtx == nil {
		return
	}
	if s.streamRetry != nil {
		s.streamRetry.Stop()
	}
	ctx := s.sessionCtx
	s.streamRetry = time.AfterFunc(wait, func() { s.reconcilePlaylistStreams(ctx, client) })
}

// handleViewerRevocation matches a revocation against the stream being
// received: a refused or expired request, or the end of the current grant.
func (s *Service) handleViewerRevocation(client *faroclient.Client, revoked protocol.MediaStreamRevoked) {
	s.mu.RLock()
	stream := s.stream
	var attempt *streamAttempt
	granted := false
	if stream != nil {
		if stream.attempt != nil && stream.attempt.requestID == revoked.RequestID {
			attempt = stream.attempt
		}
		granted = stream.grantID != "" && stream.grantID == revoked.RequestID
	}
	s.mu.RUnlock()
	cause := errors.New(revoked.Reason)
	switch {
	case attempt != nil:
		s.streamAttemptFailed(client, stream, attempt, cause)
	case granted:
		s.upstreamLost(client, stream, revoked.RequestID, false, cause)
	}
}

// streamCacheRanges bounds the cached ranges sent to the seek bar each second.
const streamCacheRanges = 48

// SetStreamCacheLimit chooses how later streams are cached on this device: a
// positive limit caps the disk cache, 0 caches whole files and a negative
// value keeps streams in memory only. A running stream keeps its cache.
func (s *Service) SetStreamCacheLimit(limitBytes int64) {
	s.mu.Lock()
	s.streamCache = cacheOptions(limitBytes)
	s.mu.Unlock()
}

func cacheOptions(limitBytes int64) mediastream.CacheOptions {
	return mediastream.CacheOptions{DiskLimit: max(limitBytes, 0), MemoryOnly: limitBytes < 0}
}

// streamStatusLocked describes stream for the page.
func (s *Service) streamStatusLocked(stream *viewerStream) StreamStatus {
	status := StreamStatus{State: "requesting", OfferID: stream.offerID}
	if stream.attempt != nil && stream.phase == streamRequesting {
		status.OfferID = stream.attempt.offerID
	}
	if stream.phase != streamActive && stream.phase != streamResuming || stream.gateway == nil {
		return status
	}
	progress := stream.gateway.Progress()
	status.State, status.Error = "active", progress.Error
	status.CachedBytes, status.TotalBytes = progress.CachedBytes, progress.TotalBytes
	switch {
	case stream.cached || progress.Complete && stream.grantID == "":
		status.Route = "cached"
	case stream.phase == streamResuming:
		status.Route = "reconnecting"
	case progress.Route == mediastream.RouteUnavailable:
		status.Route = "unavailable"
	default:
		status.Route = "direct"
	}
	return status
}

func (s *Service) emitStreamStatus(stream *viewerStream) {
	s.mu.RLock()
	if s.stream != stream {
		s.mu.RUnlock()
		return
	}
	status := s.streamStatusLocked(stream)
	s.mu.RUnlock()
	status.TotalBytes, status.CachedBytes = 0, 0
	s.sink(Event{Kind: "stream", Stream: &status})
}

// reportStreamProgress emits the stream's cache progress about once a
// second while it plays. Once the whole file is cached, only changes of
// route are reported.
func (s *Service) reportStreamProgress(stream *viewerStream, gateway *mediastream.Gateway) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last, lastAt := gateway.Progress().ReceivedBytes, time.Now()
	var previous StreamStatus
	for {
		select {
		case <-stream.ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.RLock()
		if s.stream != stream {
			s.mu.RUnlock()
			return
		}
		status := s.streamStatusLocked(stream)
		s.mu.RUnlock()
		received, now := gateway.Progress().ReceivedBytes, time.Now()
		status.BytesPerSecond = float64(received-last) / now.Sub(lastAt).Seconds()
		last, lastAt = received, now
		complete := status.TotalBytes > 0 && status.CachedBytes >= status.TotalBytes
		if complete {
			status.BytesPerSecond = 0
			if previous.CachedBytes >= previous.TotalBytes && previous.TotalBytes > 0 && previous.Route == status.Route {
				continue
			}
		}
		if status.TotalBytes == 0 {
			continue
		}
		status.CachedRanges = gateway.CachedRanges(streamCacheRanges)
		previous = status
		s.sink(Event{Kind: "stream", Stream: &status})
	}
}

// streamLogf prefixes a stream's gateway diagnostics with its identity.
func (s *Service) streamLogf(stream *viewerStream) func(string, ...any) {
	return func(format string, args ...any) {
		s.log("stream %d: "+format, append([]any{stream.id}, args...)...)
	}
}

// pausePlayerIfCurrent pauses mediaPlayer after the media it showed went
// away, unless something newer was loaded since.
func (s *Service) pausePlayerIfCurrent(mediaPlayer player.Player, gen uint64) {
	if mediaPlayer == nil {
		return
	}
	s.mu.RLock()
	current := s.player == mediaPlayer && s.mediaGen == gen
	s.mu.RUnlock()
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(s.root, 3*time.Second)
	_ = mediaPlayer.SetPaused(ctx, true)
	cancel()
}

// detachStreamLocked takes the received stream away from a session that is
// ending. The caller holds s.mu and calls the returned function after
// releasing it; the server is not told, since the session is gone.
func (s *Service) detachStreamLocked() func() {
	stream := s.stream
	s.stream = nil
	s.streamSuppressedItem = ""
	clear(s.streamFailures)
	if s.streamRetry != nil {
		s.streamRetry.Stop()
		s.streamRetry = nil
	}
	if stream == nil {
		return func() {}
	}
	stream.cancel()
	if stream.resume != nil {
		stream.resume.Stop()
	}
	attempt, gateway := stream.attempt, stream.gateway
	stream.attempt, stream.grantID = nil, ""
	if gateway != nil && s.currentSource == gateway.URL() {
		s.invalidateMediaLocked()
	}
	return func() {
		if attempt != nil {
			attempt.viewer.Close()
		}
		if gateway != nil {
			gateway.Close()
		}
	}
}

func mediaLabel(media protocol.Media) string {
	fingerprint := media.Fingerprint
	if len(fingerprint) > 16 {
		fingerprint = fingerprint[:16]
	}
	return fmt.Sprintf("%s (%d bytes)", fingerprint, media.SizeBytes)
}

func selectedPlaylistItem(playlist protocol.Playlist) (protocol.PlaylistItem, bool) {
	if playlist.Selected < 0 || playlist.Selected >= len(playlist.Items) {
		return protocol.PlaylistItem{}, false
	}
	return playlist.Items[playlist.Selected], true
}
