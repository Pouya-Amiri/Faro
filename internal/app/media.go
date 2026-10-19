package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/mediaid"
	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// mediaOperationTimeout bounds one attempt to load media into the player,
// including resolving a URL.
const mediaOperationTimeout = 90 * time.Second

// mediaLoad is one attempt to put a source in the player. Starting a load
// supersedes the previous one: its context is cancelled, so it stops at its
// next step, and it can no longer commit. Loads run one at a time under
// mediaLoadMu, but a superseded one gives way promptly instead of making the
// next selection wait for it.
//
// What the player shows is versioned separately by Service.mediaGen, which
// advances whenever a load commits or the shown media goes away (the player
// closed, the file ended, the stream it played was retired). Media is
// published under the generation it belongs to, so only the latest change
// reaches the room.
type mediaLoad struct {
	key    string // what it loads, to recognize a duplicate request
	ctx    context.Context
	cancel context.CancelFunc
	// done is guarded by Service.mu; transition belongs to the goroutine
	// running the load.
	done       bool
	transition bool
}

// beginLoad starts a load for key, superseding the load in progress. It
// returns nil when a load for the same key is already in progress, so a
// repeated room event does not restart it.
func (s *Service) beginLoad(parent context.Context, key string) *mediaLoad {
	ctx, cancel := context.WithTimeout(parent, mediaOperationTimeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load != nil && !s.load.done && s.load.key == key && s.load.ctx.Err() == nil {
		cancel()
		return nil
	}
	if s.load != nil {
		s.load.cancel()
	}
	s.load = &mediaLoad{key: key, ctx: ctx, cancel: cancel}
	return s.load
}

// cancelLoadLocked stops the load in progress, for a player that went away.
func (s *Service) cancelLoadLocked() {
	if s.load != nil {
		s.load.cancel()
	}
}

// endLoad releases a load. The caller still holds mediaLoadMu, so an
// unfinished media transition is ended before the next load can start one.
func (s *Service) endLoad(load *mediaLoad) {
	load.cancel()
	if load.transition {
		load.transition = false
		s.endMediaTransition()
	}
	s.mu.Lock()
	load.done = true
	s.mu.Unlock()
}

// loadOwnsPlayerLocked reports whether load may still commit: no newer load
// superseded it, and the session and player it loaded into are current.
func (s *Service) loadOwnsPlayerLocked(load *mediaLoad, client *faroclient.Client, mediaPlayer player.Player) bool {
	return load.ctx.Err() == nil && s.load == load && s.client == client && s.player == mediaPlayer
}

// invalidateMediaLocked records that the player no longer shows what was
// last committed: the player closed, the file ended, or the stream it
// played was retired. The selection is cleared so that playing the item
// again reloads it. A load in progress is newer than what went away and is
// left alone. It returns the new media generation.
func (s *Service) invalidateMediaLocked() uint64 {
	s.mediaGen++
	s.selectedItem, s.selectedItemIdentity = "", ""
	s.currentSource, s.currentPlayerSource = "", ""
	s.sponsorSegments, s.lastSponsorEnd = nil, 0
	return s.mediaGen
}

// publishMedia tells the room what this participant's player shows. gen is
// the media generation the information belongs to: once anything newer has
// been loaded or the player's media was invalidated, it is dropped, so an
// abandoned load can never overwrite newer state. Publications are
// serialized, so the room receives them in generation order.
func (s *Service) publishMedia(client *faroclient.Client, gen uint64, media *protocol.Media) error {
	s.mediaPublishMu.Lock()
	defer s.mediaPublishMu.Unlock()
	s.mu.RLock()
	current := s.client == client && s.mediaGen == gen
	s.mu.RUnlock()
	if !current {
		return nil
	}
	return client.SetMedia(protocol.MediaSet{Media: media})
}

// loadResult is what loadSource put in the player.
type loadResult struct {
	source       string // the logical source: a path, gateway URL or video page URL
	playerSource string // what the player reports, such as a resolved CDN URL
	info         youtube.Info
	state        player.State
}

// loadSource opens source in mediaPlayer for load and moves it to the room's
// position and rate. It starts the media transition that finishLoad ends.
// If the load fails while it is still the newest, the selection is cleared:
// opening may already have unloaded what was playing, so playing that again
// must reload it.
func (s *Service) loadSource(load *mediaLoad, client *faroclient.Client, mediaPlayer player.Player, source string) (loadResult, error) {
	s.beginMediaTransition()
	load.transition = true
	result, err := s.openSource(load, client, mediaPlayer, source)
	if err != nil {
		s.mu.Lock()
		if s.load == load && s.player == mediaPlayer {
			s.selectedItem, s.selectedItemIdentity = "", ""
			s.currentSource, s.currentPlayerSource = "", ""
		}
		s.mu.Unlock()
	}
	return result, err
}

func (s *Service) openSource(load *mediaLoad, client *faroclient.Client, mediaPlayer player.Player, source string) (loadResult, error) {
	info, playerSource, err := s.openPlayerSource(load.ctx, mediaPlayer, source)
	if err != nil {
		return loadResult{}, err
	}
	state, err := s.waitForPlayerMedia(load.ctx, mediaPlayer, info.Duration)
	if err != nil {
		return loadResult{}, err
	}
	playback := localClockSnapshot(client).Playback
	_ = mediaPlayer.Seek(load.ctx, playback.PositionSeconds)
	_ = mediaPlayer.SetRate(load.ctx, normalizedRate(playback.Rate))
	if err := load.ctx.Err(); err != nil {
		return loadResult{}, err
	}
	if playerSource == "" {
		playerSource = source
	}
	return loadResult{source: source, playerSource: playerSource, info: info, state: state}, nil
}

// commitSourceLocked records a committed load's source as what the player
// shows and returns the media generation to publish it under.
func (s *Service) commitSourceLocked(result loadResult) uint64 {
	s.currentSource, s.currentPlayerSource = result.source, result.playerSource
	s.sponsorSegments = append([]youtube.Segment(nil), result.info.Segments...)
	s.lastSponsorEnd = 0
	s.mediaGen++
	return s.mediaGen
}

// finishLoad ends a committed load's media transition: the player takes the
// newest pause state and publish, if given, announces it atomically with
// queued local controls.
func (s *Service) finishLoad(load *mediaLoad, mediaPlayer player.Player, fallbackPaused bool, publish func(bool) error) (bool, error) {
	load.transition = false
	return s.finishMediaTransition(load.ctx, mediaPlayer, fallbackPaused, publish)
}

// applySelectedPlaylist makes the player show the room's selected queue item
// and returns once it is loaded (or nothing needs loading).
func (s *Service) applySelectedPlaylist(ctx context.Context, client *faroclient.Client) {
	if plan, ok := s.planSelectedLoad(ctx, client); ok {
		s.loadSelectedItem(client, plan)
	}
}

// startSelectedPlaylist is applySelectedPlaylist for room events: the load
// runs in the background, so the next event can supersede a slow one.
func (s *Service) startSelectedPlaylist(ctx context.Context, client *faroclient.Client) {
	if plan, ok := s.planSelectedLoad(ctx, client); ok {
		go s.loadSelectedItem(client, plan)
	}
}

// selectedLoad is a decision to load a queue item.
type selectedLoad struct {
	load     *mediaLoad
	item     protocol.PlaylistItem
	identity string
	source   string
}

// planSelectedLoad decides what the selection needs and, when that is a
// load, starts it, superseding any other. Decisions are serialized and each
// reads the latest selection, so an older decision can never supersede a
// newer one.
func (s *Service) planSelectedLoad(ctx context.Context, client *faroclient.Client) (selectedLoad, bool) {
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	if ctx.Err() != nil {
		return selectedLoad{}, false
	}
	s.mu.RLock()
	current := s.client == client
	s.mu.RUnlock()
	if !current {
		return selectedLoad{}, false
	}
	item, ok := selectedPlaylistItem(client.Snapshot().Playlist)
	if !ok {
		s.stopSelectedPlayback(client, "")
		return selectedLoad{}, false
	}
	identity := playlistItemIdentity(item)
	s.mu.RLock()
	selected := item.ID == s.selectedItem && identity == s.selectedItemIdentity
	s.mu.RUnlock()
	if selected {
		return selectedLoad{}, false
	}
	source := item.URL
	if source == "" && item.Media != nil {
		source = s.localSource(item.Media.Fingerprint)
	}
	if source == "" {
		s.stopSelectedPlayback(client, item.ID)
		// Someone who has the file shares it (or is about to), and the stream
		// starts on its own; only a file nobody can provide is an error.
		fingerprint := ""
		if item.Media != nil {
			fingerprint = item.Media.Fingerprint
		}
		if !(s.StreamingAvailable() && awaitingSharedCopy(client.Snapshot(), fingerprint)) {
			s.sink(Event{Kind: "error", Error: &protocol.Error{
				Code: "media_missing", Message: fmt.Sprintf("Locate a local copy of %q before playing it", item.Label),
			}})
		}
		return selectedLoad{}, false
	}
	load := s.beginLoad(ctx, "item:"+item.ID+"|"+identity)
	if load == nil {
		return selectedLoad{}, false
	}
	return selectedLoad{load: load, item: item, identity: identity, source: source}, true
}

func (s *Service) loadSelectedItem(client *faroclient.Client, plan selectedLoad) {
	load, item, identity, source := plan.load, plan.item, plan.identity, plan.source
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	defer s.endLoad(load)
	if load.ctx.Err() != nil {
		return
	}
	_, mediaPlayer, err := s.ensurePlayer(playerStartAutomatic)
	if err != nil {
		if !errors.Is(err, errPlayerDismissed) {
			s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "open_player", Message: err.Error()}})
		}
		return
	}
	result, err := s.loadSource(load, client, mediaPlayer, source)
	if err != nil {
		if load.ctx.Err() == nil {
			s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "open_media", Message: err.Error()}})
		}
		return
	}
	media := cloneStreamMedia(item.Media)
	if media == nil {
		media, _ = s.identifyAndRemember(source, item.Label, firstPositive(result.info.Duration, result.state.DurationSeconds))
	}
	if media != nil {
		if result.info.Title != "" {
			media.Title = result.info.Title
		}
		if duration := firstPositive(result.info.Duration, result.state.DurationSeconds); duration > 0 {
			media.DurationSeconds = duration
		}
	}
	s.mu.Lock()
	latest, ok := selectedPlaylistItem(client.Snapshot().Playlist)
	if !s.loadOwnsPlayerLocked(load, client, mediaPlayer) || !ok || latest.ID != item.ID || playlistItemIdentity(latest) != identity {
		// A newer selection supersedes this load and takes over the player.
		s.mu.Unlock()
		return
	}
	gen := s.commitSourceLocked(result)
	s.selectedItem, s.selectedItemIdentity = item.ID, identity
	s.manualSource = ""
	s.mu.Unlock()
	if media != nil {
		_ = s.publishMedia(client, gen, media)
	}
	// Playback may have changed while loading (notably when Pause is pressed
	// just after a wheel spin). Re-read the authoritative state at the last
	// possible moment and finish atomically with queued local controls.
	if _, err := s.finishLoad(load, mediaPlayer, localClockSnapshot(client).Playback.Paused, nil); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "player_sync", Message: err.Error()}})
	}
}

// stopSelectedPlayback retires what the player shows for a queue item other
// than selectedID ("" when nothing is selected), which has no local copy to
// play: a committed item, or one still loading. Media opened outside the
// queue is left alone, and so is a stream taking over the player for the
// selection.
func (s *Service) stopSelectedPlayback(client *faroclient.Client, selectedID string) {
	s.mu.Lock()
	if s.client != client {
		s.mu.Unlock()
		return
	}
	load := s.load
	inProgress := load != nil && !load.done && load.ctx.Err() == nil
	if inProgress && strings.HasPrefix(load.key, "stream:") {
		s.mu.Unlock()
		return
	}
	staleLoad := inProgress && strings.HasPrefix(load.key, "item:") && !strings.HasPrefix(load.key, "item:"+selectedID+"|")
	if staleLoad {
		load.cancel()
	}
	mediaPlayer := s.player
	release := mediaPlayer != nil && (staleLoad || s.selectedItem != "" && s.selectedItem != selectedID)
	s.mu.Unlock()
	if release {
		s.releasePlayer(mediaPlayer, "", false)
	}
}

// SetPlaylist replaces the room's queue. With baseRevision set it applies
// only if the queue is still at that revision, so a whole-queue edit computed
// from what this participant saw (a shuffle, an undo) never discards a change
// someone else made meanwhile; it fails with a conflict instead.
func (s *Service) SetPlaylist(inputs []PlaylistInput, baseRevision *uint64) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	items, skipped, err := s.resolvePlaylistInputs(inputs)
	if err != nil {
		return err
	}
	if err := s.publishPlaylist(client, items, baseRevision); err != nil {
		return err
	}
	s.reportSkippedMedia(skipped)
	return nil
}

// AppendPlaylist adds items to the end of the room's queue and reports how
// many were added. The new items are inspected first and appended to the
// latest queue, so changes other participants made in the meantime are kept.
// With play set, the first new item is selected.
func (s *Service) AppendPlaylist(inputs []PlaylistInput, play bool) (int, error) {
	client, err := s.connected()
	if err != nil {
		return 0, err
	}
	additions, skipped, err := s.resolvePlaylistInputs(inputs)
	if err != nil {
		return 0, err
	}
	if len(additions) == 0 {
		return 0, errors.New("nothing to add")
	}
	// IDs chosen here let the new item be selected wherever it ends up.
	for index := range additions {
		if additions[index].ID == "" {
			if additions[index].ID, err = newPlaylistItemID(); err != nil {
				return 0, err
			}
		}
	}
	err = s.editPlaylist(client, func(items []protocol.PlaylistItem) ([]protocol.PlaylistItem, error) {
		return append(items, additions...), nil
	})
	if err != nil {
		return 0, err
	}
	s.reportSkippedMedia(skipped)
	if play {
		if err := client.SelectPlaylist(protocol.PlaylistSelect{ItemID: additions[0].ID, Index: len(client.Snapshot().Playlist.Items) - len(additions)}); err != nil {
			return len(additions), err
		}
	}
	return len(additions), nil
}

// RemovePlaylistItem removes an item from the queue wherever it is now. An
// item someone else already removed is not an error.
func (s *Service) RemovePlaylistItem(itemID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return s.editPlaylist(client, func(items []protocol.PlaylistItem) ([]protocol.PlaylistItem, error) {
		return slices.DeleteFunc(items, func(item protocol.PlaylistItem) bool { return item.ID == itemID }), nil
	})
}

// MovePlaylistItem moves an item to the place another item holds now, as
// dragging a row onto another does.
func (s *Service) MovePlaylistItem(itemID, targetID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return s.editPlaylist(client, func(items []protocol.PlaylistItem) ([]protocol.PlaylistItem, error) {
		from := slices.IndexFunc(items, func(item protocol.PlaylistItem) bool { return item.ID == itemID })
		to := slices.IndexFunc(items, func(item protocol.PlaylistItem) bool { return item.ID == targetID })
		if from < 0 || to < 0 {
			return nil, errors.New("that queue item was removed")
		}
		item := items[from]
		items = slices.Delete(items, from, from+1)
		return slices.Insert(items, to, item), nil
	})
}

// playlistEditAttempts bounds how often an edit is rebased onto a queue that
// keeps changing under it.
const playlistEditAttempts = 5

// editPlaylist applies change to the latest queue and publishes it on the
// condition that the queue has not changed meanwhile. When it has, the edit
// is redone on the newer queue, so an edit is never lost to another one.
func (s *Service) editPlaylist(client *faroclient.Client, change func([]protocol.PlaylistItem) ([]protocol.PlaylistItem, error)) error {
	for attempt := 1; ; attempt++ {
		playlist := client.Snapshot().Playlist
		items := make([]protocol.PlaylistItem, 0, len(playlist.Items)+1)
		for _, item := range playlist.Items {
			item.Media = cloneStreamMedia(item.Media)
			items = append(items, item)
		}
		items, err := change(items)
		if err != nil {
			return err
		}
		revision := playlist.Revision
		err = s.publishPlaylist(client, items, &revision)
		var commandErr *faroclient.CommandError
		if err == nil || !errors.As(err, &commandErr) || commandErr.Code != protocol.ErrorPlaylistConflict || attempt == playlistEditAttempts {
			return err
		}
		waitForPlaylistRevision(client, revision)
	}
}

// waitForPlaylistRevision waits briefly until the newer queue that caused a
// conflict has arrived.
func waitForPlaylistRevision(client *faroclient.Client, stale uint64) {
	deadline := time.Now().Add(2 * time.Second)
	for client.Snapshot().Playlist.Revision == stale && time.Now().Before(deadline) {
		select {
		case <-client.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func newPlaylistItemID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

// skippedMedia is a queue entry that could not be added, such as a file that
// was moved or cannot be read.
type skippedMedia struct {
	source string
	err    error
}

// resolvePlaylistInputs turns the page's queue entries into room items,
// identifying new local files and URLs. A local file that cannot be read is
// skipped instead of failing the whole batch; it is an error only when
// nothing new could be added at all.
func (s *Service) resolvePlaylistInputs(inputs []PlaylistInput) ([]protocol.PlaylistItem, []skippedMedia, error) {
	items := make([]protocol.PlaylistItem, 0, len(inputs))
	probes := make(map[int]string)
	var skipped []skippedMedia
	added := 0
	for _, input := range inputs {
		item := protocol.PlaylistItem{ID: input.ID, Label: strings.TrimSpace(input.Label), URL: input.URL, Media: cloneStreamMedia(input.Media)}
		localPath := ""
		if input.Source != "" || input.URL != "" {
			source := firstNonEmpty(input.Source, input.URL)
			result, inspectErr := mediaid.Inspect(source, item.Label, max(input.DurationSeconds, 0))
			if inspectErr != nil {
				skipped = append(skipped, skippedMedia{source: source, err: inspectErr})
				continue
			}
			added++
			item.URL, item.Media = result.URL, &result.Media
			if result.Path != "" {
				s.rememberSource(result.Media.Fingerprint, result.Path)
				localPath = result.Path
			}
		}
		if item.Label == "" && item.Media != nil {
			item.Label = item.Media.Title
		}
		// Fill in durations the queue does not know yet: new local files, and
		// items already in the queue for which this participant has a copy.
		if item.Media != nil && item.Media.DurationSeconds <= 0 {
			if input.DurationSeconds > 0 {
				item.Media.DurationSeconds = input.DurationSeconds
			} else if localPath == "" && item.URL == "" {
				localPath = s.localSource(item.Media.Fingerprint)
			}
			if item.Media.DurationSeconds <= 0 && localPath != "" {
				probes[len(items)] = localPath
			}
		}
		items = append(items, item)
	}
	if len(skipped) != 0 && added == 0 {
		return nil, nil, skipped[0].err
	}
	probePlaylistDurations(items, probes)
	return items, skipped, nil
}

// publishPlaylist sends the queue. The update it causes reaches this
// participant like everyone else, and that is what stops playback of a
// removed item; waiting for a load in progress here would only make the
// edit look busy.
func (s *Service) publishPlaylist(client *faroclient.Client, items []protocol.PlaylistItem, baseRevision *uint64) error {
	if err := client.SetPlaylist(protocol.PlaylistSet{Items: items, BaseRevision: baseRevision}); err != nil {
		return err
	}
	if err := s.withdrawMissingOffer(client, items); err != nil {
		return err
	}
	// The event performs the same transfer reconciliation for every participant.
	go s.reconcilePlaylistStreams(s.root, client)
	return nil
}

func skipReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the file no longer exists"
	case errors.Is(err, fs.ErrPermission):
		return "Faro is not allowed to read it"
	}
	return err.Error()
}

// reportSkippedMedia tells the page which entries of an otherwise successful
// batch were left out.
func (s *Service) reportSkippedMedia(skipped []skippedMedia) {
	if len(skipped) == 0 {
		return
	}
	name := filepath.Base(skipped[0].source)
	message := fmt.Sprintf("%s could not be added: %s", name, skipReason(skipped[0].err))
	if len(skipped) > 1 {
		message = fmt.Sprintf("%d files could not be added because they are missing or unreadable, including %s", len(skipped), name)
	}
	s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "playlist_skipped", Message: message}})
}

// probePlaylistDurations reads durations for the given item indexes in
// parallel. Container headers are read directly and are fast; the ffprobe
// fallback for other formats is not, so a large folder drop is spread out.
func probePlaylistDurations(items []protocol.PlaylistItem, paths map[int]string) {
	if len(paths) == 0 {
		return
	}
	type job struct {
		index int
		path  string
	}
	jobs := make(chan job)
	var wait sync.WaitGroup
	for range min(8, len(paths)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for next := range jobs {
				if duration := mediaid.ProbeDuration(next.path); duration > 0 {
					items[next.index].Media.DurationSeconds = duration
				}
			}
		}()
	}
	for index, path := range paths {
		jobs <- job{index: index, path: path}
	}
	close(jobs)
	wait.Wait()
}

// SelectPlaylist selects the queue item with itemID wherever it is now; index
// is used on its own only when itemID is empty or the server predates IDs.
func (s *Service) SelectPlaylist(index int, itemID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	s.resumeStreamingIntent()
	return client.SelectPlaylist(protocol.PlaylistSelect{Index: index, ItemID: itemID})
}

func (s *Service) SpinPlaylistWheel() error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	return client.SpinPlaylistWheel()
}

// reopenMediaAtRoomClock opens source at the room's position, for Play
// pressed while no player shows anything.
func (s *Service) reopenMediaAtRoomClock(source string, paused bool) error {
	s.mu.RLock()
	sessionCtx := s.sessionCtx
	s.mu.RUnlock()
	if sessionCtx == nil {
		return errors.New("not connected")
	}
	load := s.beginLoad(sessionCtx, "reopen:"+source)
	if load == nil {
		return nil
	}
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	defer s.endLoad(load)
	client, mediaPlayer, err := s.ensurePlayer(playerStartExplicit)
	if err != nil {
		return err
	}
	identity, err := mediaid.Inspect(source, "", 0)
	if err != nil {
		return err
	}
	s.mu.RLock()
	if stream := s.stream; stream != nil && stream.gateway != nil && source == stream.gateway.URL() {
		identity.Media = stream.media
	}
	s.mu.RUnlock()
	_, queued := selectedPlaylistItem(client.Snapshot().Playlist)
	queueSource := queued && s.sourceForPlayback(client) == source
	result, err := s.loadSource(load, client, mediaPlayer, source)
	if err != nil {
		if load.ctx.Err() != nil {
			return errors.New("opening media was interrupted by another selection")
		}
		return err
	}
	if result.info.Title != "" {
		identity.Media.Title = result.info.Title
	} else if result.state.Title != "" && identity.Media.Title == "" {
		identity.Media.Title = result.state.Title
	}
	if duration := firstPositive(result.info.Duration, result.state.DurationSeconds); duration > 0 {
		identity.Media.DurationSeconds = duration
	}
	if identity.Path != "" {
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
	}
	itemID, itemIdentity := "", ""
	if item, ok := selectedPlaylistItem(client.Snapshot().Playlist); ok && s.sourceForPlayback(client) == source {
		itemID, itemIdentity = item.ID, playlistItemIdentity(item)
	}
	s.mu.Lock()
	if !s.loadOwnsPlayerLocked(load, client, mediaPlayer) || queueSource && itemID == "" {
		s.mu.Unlock()
		return errors.New("selected item changed while opening media")
	}
	gen := s.commitSourceLocked(result)
	s.selectedItem, s.selectedItemIdentity = itemID, itemIdentity
	if itemID == "" {
		s.manualSource = source
	} else {
		s.manualSource = ""
	}
	s.mu.Unlock()
	playback := localClockSnapshot(client).Playback
	_, err = s.finishLoad(load, mediaPlayer, paused, func(finalPaused bool) error {
		if err := s.publishMedia(client, gen, &identity.Media); err != nil {
			return err
		}
		return client.SetPlayback(protocol.PlaybackSet{
			PositionSeconds: playback.PositionSeconds, Paused: finalPaused,
			Rate: normalizedRate(playback.Rate), Seek: true,
		})
	})
	return err
}

func (s *Service) LocatePlaylistItem(itemID, source string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	snapshot := client.Snapshot()
	var wanted *protocol.PlaylistItem
	for index := range snapshot.Playlist.Items {
		if snapshot.Playlist.Items[index].ID == itemID {
			wanted = &snapshot.Playlist.Items[index]
			break
		}
	}
	if wanted == nil || wanted.Media == nil {
		return errors.New("playlist item has no local media identity")
	}
	identity, err := mediaid.Inspect(source, wanted.Label, wanted.Media.DurationSeconds)
	if err != nil {
		return err
	}
	if identity.Media.Fingerprint != wanted.Media.Fingerprint {
		return errors.New("selected file does not match the room playlist item")
	}
	s.rememberSource(identity.Media.Fingerprint, identity.Path)
	// A local copy replaces the stream of the same file, whatever stage the
	// stream is at.
	s.mu.RLock()
	stream, sessionCtx := s.stream, s.sessionCtx
	s.mu.RUnlock()
	if stream != nil && stream.media.Fingerprint == identity.Media.Fingerprint {
		s.retireStream(client, stream, nil)
	}
	if sessionCtx != nil {
		s.applySelectedPlaylist(sessionCtx, client)
	}
	return nil
}

func (s *Service) beginMediaTransition() {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.mu.Lock()
	s.suppressUntil = time.Now().Add(3 * time.Second)
	s.openingMedia = true
	s.transitionPaused = nil
	s.lastSponsorEnd = 0
	s.mu.Unlock()
}

func (s *Service) endMediaTransition() {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.clearMediaTransition()
}

func (s *Service) clearMediaTransition() {
	s.mu.Lock()
	s.suppressUntil = time.Time{}
	s.openingMedia = false
	s.transitionPaused = nil
	s.mu.Unlock()
}

func (s *Service) queueTransitionPause(paused bool) bool {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.openingMedia {
		return false
	}
	value := paused
	s.transitionPaused = &value
	return true
}

// finishMediaTransition serializes the last player command and authoritative
// publication with SetPaused. A pause that arrives before this method is
// queued and wins over the stale state captured when loading began; a pause
// that arrives afterwards observes openingMedia=false and runs normally.
func (s *Service) finishMediaTransition(ctx context.Context, mediaPlayer player.Player, fallbackPaused bool, publish func(bool) error) (bool, error) {
	s.mediaTransitionMu.Lock()
	defer s.mediaTransitionMu.Unlock()
	s.mu.RLock()
	queued := s.transitionPaused
	s.mu.RUnlock()
	paused := fallbackPaused
	if queued != nil {
		paused = *queued
	}
	err := (&trackedPlayer{Player: mediaPlayer, service: s}).SetPaused(ctx, paused)
	if err == nil && publish != nil {
		err = publish(paused)
	}
	s.clearMediaTransition()
	return paused, err
}

// openPlayerSource opens source in mediaPlayer, resolving a YouTube page to
// its streams first. It returns what the player reports as its source when
// that differs from source, such as a resolved CDN URL.
func (s *Service) openPlayerSource(ctx context.Context, mediaPlayer player.Player, source string) (youtube.Info, string, error) {
	if !youtube.IsURL(source) {
		return youtube.Info{}, "", mediaPlayer.Open(ctx, source)
	}
	s.mu.RLock()
	height := s.youtubeQualities[source]
	s.mu.RUnlock()
	stream, err := s.youtube.Resolve(ctx, source, height)
	if err != nil {
		return youtube.Info{}, "", err
	}
	if resolvedPlayer, ok := mediaPlayer.(player.ResolvedStreamPlayer); ok {
		err = resolvedPlayer.OpenResolved(ctx, player.ResolvedStream{
			VideoURL: stream.VideoURL, AudioURL: stream.AudioURL, CombinedURL: stream.CombinedURL, Title: stream.Info.Title,
		})
	} else if stream.CombinedURL != "" {
		err = mediaPlayer.Open(ctx, stream.CombinedURL)
	} else {
		err = errors.New("this player cannot combine separate high-quality YouTube video and audio streams")
	}
	if err != nil {
		if ctx.Err() != nil {
			return youtube.Info{}, "", ctx.Err()
		}
		fallbackResolver, supported := s.youtube.(interface {
			ResolveFallback(context.Context, string, int) (youtube.Stream, error)
		})
		if !supported {
			return youtube.Info{}, "", err
		}
		fallback, fallbackErr := fallbackResolver.ResolveFallback(ctx, source, height)
		if fallbackErr != nil {
			return youtube.Info{}, "", fmt.Errorf("high-quality YouTube stream failed to open (%v); web_safari fallback failed: %w", err, fallbackErr)
		}
		if resolvedPlayer, ok := mediaPlayer.(player.ResolvedStreamPlayer); ok {
			fallbackErr = resolvedPlayer.OpenResolved(ctx, player.ResolvedStream{CombinedURL: fallback.CombinedURL, Title: fallback.Info.Title})
		} else {
			fallbackErr = mediaPlayer.Open(ctx, fallback.CombinedURL)
		}
		if fallbackErr != nil {
			return youtube.Info{}, "", fmt.Errorf("web_safari fallback failed to open: %w", fallbackErr)
		}
		stream = fallback
	}
	loadedState, _ := mediaPlayer.State(ctx)
	return stream.Info, loadedState.Source, nil
}

func (s *Service) YouTubeInfo(source string) (youtube.Info, error) {
	if !youtube.IsURL(source) {
		return youtube.Info{}, errors.New("a YouTube URL is required")
	}
	ctx, cancel := s.mediaOperationContext(s.root)
	defer cancel()
	return s.youtube.Inspect(ctx, source)
}

// YouTubeSupport reports whether this device can play YouTube links.
func (s *Service) YouTubeSupport() youtube.Support {
	checker, supported := s.youtube.(interface {
		Support(context.Context) youtube.Support
	})
	if !supported {
		return youtube.Support{}
	}
	ctx, cancel := context.WithTimeout(s.root, 10*time.Second)
	defer cancel()
	return checker.Support(ctx)
}

func (s *Service) SetYouTubeQuality(source string, height int) error {
	if !youtube.IsURL(source) {
		return errors.New("a YouTube URL is required")
	}
	if height < 0 || height > 8640 {
		return errors.New("YouTube quality must be Auto or a valid video height")
	}
	s.mu.Lock()
	s.youtubeQualities[source] = height
	s.mu.Unlock()
	return nil
}

// ChangeYouTubeQuality reloads the video playing at another quality. The
// player returns to the room's position, so the change never moves anyone
// else; a newer selection made meanwhile supersedes it.
func (s *Service) ChangeYouTubeQuality(source string, height int) error {
	if err := s.SetYouTubeQuality(source, height); err != nil {
		return err
	}
	s.mu.RLock()
	client, mediaPlayer, current, sessionCtx := s.client, s.player, s.currentSource, s.sessionCtx
	s.mu.RUnlock()
	if client == nil || mediaPlayer == nil || current != source || sessionCtx == nil {
		return nil
	}
	load := s.beginLoad(sessionCtx, fmt.Sprintf("quality:%s:%d", source, height))
	if load == nil {
		return nil
	}
	s.mediaLoadMu.Lock()
	defer s.mediaLoadMu.Unlock()
	defer s.endLoad(load)
	s.mu.RLock()
	still := s.currentSource == source && s.player == mediaPlayer && load.ctx.Err() == nil
	s.mu.RUnlock()
	if !still {
		return nil
	}
	result, err := s.loadSource(load, client, mediaPlayer, source)
	if err != nil {
		if load.ctx.Err() != nil {
			return nil
		}
		return err
	}
	s.mu.Lock()
	if !s.loadOwnsPlayerLocked(load, client, mediaPlayer) {
		s.mu.Unlock()
		return nil
	}
	s.commitSourceLocked(result)
	// Reloading cleared the selection in case it failed; it succeeded.
	if item, ok := selectedPlaylistItem(client.Snapshot().Playlist); ok && item.URL == source {
		s.selectedItem, s.selectedItemIdentity = item.ID, playlistItemIdentity(item)
	}
	s.mu.Unlock()
	_, err = s.finishLoad(load, mediaPlayer, localClockSnapshot(client).Playback.Paused, nil)
	return err
}

// waitForPlayerMedia bridges the asynchronous load semantics of mpv-family
// players. Open only acknowledges the load command; duration and chapters are
// populated shortly afterwards. Publishing the first state too early leaves
// the UI with a disabled timeline until the same file is opened again. It
// fails only if ctx ends: an abandoned load must not be committed.
func (s *Service) waitForPlayerMedia(ctx context.Context, mediaPlayer player.Player, knownDuration float64) (player.State, error) {
	var latest player.State
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := mediaPlayer.State(ctx)
		if err == nil {
			latest = state
			if state.DurationSeconds > 0 || knownDuration > 0 && state.Source != "" {
				return latest, nil
			}
		}
		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-deadline.C:
			return latest, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) SetSponsorBlockEnabled(enabled bool) {
	s.mu.Lock()
	s.sponsorBlock = enabled
	s.mu.Unlock()
}

func (s *Service) TimelineSegments() []TimelineSegment {
	s.mu.RLock()
	mediaPlayer := s.player
	sponsorSegments := append([]youtube.Segment(nil), s.sponsorSegments...)
	s.mu.RUnlock()

	result := make([]TimelineSegment, 0, len(sponsorSegments)+8)
	for _, segment := range sponsorSegments {
		result = append(result, TimelineSegment{
			StartSeconds: segment.Start, EndSeconds: segment.End, Title: segment.Title,
			Category: segment.Category, Kind: "sponsorblock",
		})
	}
	if mediaPlayer != nil {
		ctx, cancel := context.WithTimeout(s.root, 2*time.Second)
		state, err := mediaPlayer.State(ctx)
		cancel()
		if err == nil {
			for index, chapter := range state.Chapters {
				end := state.DurationSeconds
				if index+1 < len(state.Chapters) {
					end = state.Chapters[index+1].StartSeconds
				}
				title := strings.TrimSpace(chapter.Title)
				if title == "" {
					title = fmt.Sprintf("Chapter %d", index+1)
				}
				result = append(result, TimelineSegment{
					StartSeconds: chapter.StartSeconds, EndSeconds: end, Title: title, Kind: "chapter",
				})
			}
		}
	}
	slices.SortFunc(result, func(a, b TimelineSegment) int {
		switch {
		case a.StartSeconds < b.StartSeconds:
			return -1
		case a.StartSeconds > b.StartSeconds:
			return 1
		default:
			return 0
		}
	})
	return result
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func (s *Service) identifyAndRemember(source, title string, duration float64) (*protocol.Media, error) {
	identity, err := mediaid.Inspect(source, title, duration)
	if err != nil {
		return nil, err
	}
	if identity.Path != "" {
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
	}
	return &identity.Media, nil
}

// localCopy is a file known to hold a media fingerprint, with the size and
// modification time it had when it was identified.
type localCopy struct {
	path     string
	size     int64
	modified time.Time
}

func (s *Service) rememberSource(fingerprint, source string) {
	if fingerprint == "" || source == "" {
		return
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	s.mu.Lock()
	s.sources[fingerprint] = localCopy{path: source, size: info.Size(), modified: info.ModTime()}
	s.mu.Unlock()
}

// localSource returns the local copy of fingerprint if it is still there
// and unchanged. A copy that was moved, deleted or modified since it was
// identified is forgotten, so playback falls back to a stream instead of
// failing, and the queue shows the item as missing.
func (s *Service) localSource(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	s.mu.RLock()
	known, ok := s.sources[fingerprint]
	s.mu.RUnlock()
	if !ok {
		return ""
	}
	if info, err := os.Stat(known.path); err == nil && info.Mode().IsRegular() && info.Size() == known.size && info.ModTime().Equal(known.modified) {
		return known.path
	}
	s.mu.Lock()
	if s.sources[fingerprint] == known {
		delete(s.sources, fingerprint)
	}
	s.mu.Unlock()
	return ""
}

func (s *Service) forgetSource(fingerprint string) {
	s.mu.Lock()
	delete(s.sources, fingerprint)
	s.mu.Unlock()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Service) PlaylistAvailability() map[string]bool {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	result := make(map[string]bool)
	if client == nil {
		return result
	}
	for _, item := range client.Snapshot().Playlist.Items {
		result[item.ID] = item.URL != "" || item.Media != nil && s.localSource(item.Media.Fingerprint) != ""
	}
	s.publishAvailability(client, result)
	return result
}

func (s *Service) PlaylistExportLines() ([]string, error) {
	client, err := s.connected()
	if err != nil {
		return nil, err
	}
	items := client.Snapshot().Playlist.Items
	lines := make([]string, 0, len(items))
	for _, item := range items {
		source := item.URL
		if source == "" && item.Media != nil {
			source = s.localSource(item.Media.Fingerprint)
		}
		if source == "" {
			source = item.Label
		}
		lines = append(lines, source)
	}
	return lines, nil
}

// IndexMediaDirectory identifies the media files in directory so queue items
// they match play locally. Indexing in a room stops when the room is left.
func (s *Service) IndexMediaDirectory(directory string) (int, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return 0, errors.New("media directory is required")
	}
	s.mu.RLock()
	ctx, client := s.sessionCtx, s.client
	s.mu.RUnlock()
	if ctx == nil {
		ctx = s.root
	}
	count := 0
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return skipUnreadable(directory, path, entry, walkErr)
		}
		if entry.IsDir() || !isMediaExtension(filepath.Ext(entry.Name())) {
			return nil
		}
		if count >= 10000 {
			return errors.New("media directory exceeds the 10000-file indexing limit")
		}
		identity, err := mediaid.Inspect(path, "", 0)
		if err != nil {
			return nil
		}
		s.rememberSource(identity.Media.Fingerprint, identity.Path)
		count++
		return nil
	})
	if err != nil {
		return count, fmt.Errorf("index media directory: %w", err)
	}
	// A joining participant commonly has no player yet, and the selected item
	// may only now have a local copy: apply the selection again. The room
	// must still be the one indexing started in.
	if client != nil {
		s.mu.RLock()
		current := s.client == client
		s.mu.RUnlock()
		if current {
			s.applySelectedPlaylist(ctx, client)
		}
	}
	return count, nil
}

func playlistItemIdentity(item protocol.PlaylistItem) string {
	if item.URL != "" {
		return "url:" + item.URL
	}
	if item.Media != nil && item.Media.Fingerprint != "" {
		return "media:" + item.Media.Fingerprint
	}
	return ""
}

func isMediaExtension(extension string) bool {
	switch strings.ToLower(extension) {
	case ".mkv", ".mp4", ".webm", ".avi", ".mov", ".m4v", ".mp3", ".m4a", ".flac", ".ogg", ".wav", ".opus":
		return true
	default:
		return false
	}
}

// skipUnreadable lets a directory walk continue past a subfolder or file it
// cannot read, such as "System Volume Information" or a folder without
// permission. Only an unreadable starting folder fails the walk.
func skipUnreadable(root, path string, entry fs.DirEntry, walkErr error) error {
	if path == root {
		return walkErr
	}
	if entry != nil && entry.IsDir() {
		return fs.SkipDir
	}
	return nil
}

func MediaFilesInDirectory(directory string, maximum int) ([]string, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return nil, errors.New("media directory is required")
	}
	if maximum <= 0 {
		maximum = 500
	}
	files := make([]string, 0)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return skipUnreadable(directory, path, entry, walkErr)
		}
		if entry.IsDir() || !isMediaExtension(filepath.Ext(entry.Name())) {
			return nil
		}
		if len(files) >= maximum {
			return fmt.Errorf("folder has more media files than the %d queue slots available", maximum)
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return files, fmt.Errorf("read media directory: %w", err)
	}
	slices.Sort(files)
	return files, nil
}

func ExpandMediaPaths(paths []string, maximum int) ([]string, error) {
	if maximum <= 0 {
		maximum = 500
	}
	files := make([]string, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect dropped media: %w", err)
		}
		if !info.IsDir() {
			if isMediaExtension(filepath.Ext(path)) {
				files = append(files, path)
			}
			continue
		}
		remaining := maximum - len(files)
		if remaining <= 0 {
			return nil, fmt.Errorf("dropped media exceeds the %d-item queue limit", maximum)
		}
		directoryFiles, err := MediaFilesInDirectory(path, remaining)
		if err != nil {
			return nil, err
		}
		files = append(files, directoryFiles...)
	}
	if len(files) > maximum {
		return nil, fmt.Errorf("dropped media exceeds the %d-item queue limit", maximum)
	}
	return files, nil
}

// Closing the player must cancel URL resolution and loading immediately,
// without cancelling the room connection.
func (s *Service) mediaOperationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, mediaOperationTimeout)
	s.mu.RLock()
	playerContext := s.playerContext
	s.mu.RUnlock()
	if playerContext == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(playerContext, cancel)
	return ctx, func() { stop(); cancel() }
}
