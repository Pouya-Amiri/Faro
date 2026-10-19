package app

import (
	"context"
	"errors"
	"math"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// publishedMedia is the media last announced to the room and the media
// generation it described.
type publishedMedia struct {
	gen   uint64
	media *protocol.Media
}

// liveProbe recognizes a live stream that its URL does not announce as one:
// its duration keeps growing at about the pace of the clock, which a video
// never does (a duration refined while a file downloads grows far faster).
type liveProbe struct {
	gen      uint64
	duration float64
	at       time.Time
}

// liveProbeWindow is how long a duration must keep growing before the media
// counts as live. playbackRecoveryDelay is the first pause before a failed
// source is reloaded; it triples with each attempt. Variables so tests can
// shorten them.
var (
	liveProbeWindow       = 10 * time.Second
	playbackRecoveryDelay = 2 * time.Second
)

// observeDurationLocked feeds a duration the player reported for media
// generation gen to the live probe and reports whether the media turned out
// to be live.
func (s *Service) observeDurationLocked(gen uint64, duration float64, now time.Time) bool {
	if s.currentLive || duration <= 0 {
		return false
	}
	probe := &s.liveProbe
	if probe.gen != gen || duration < probe.duration {
		*probe = liveProbe{gen: gen, duration: duration, at: now}
		return false
	}
	elapsed := now.Sub(probe.at).Seconds()
	if elapsed < liveProbeWindow.Seconds() {
		return false
	}
	growth := duration - probe.duration
	if growth >= elapsed/2 && growth <= elapsed*2 {
		s.currentLive = true
		return true
	}
	*probe = liveProbe{gen: gen, duration: duration, at: now}
	return false
}

// mediaUpdateLocked derives what to announce for a player event about the
// media already announced for generation gen: the duration once it becomes
// known or really changes, and the live flag once the media turns out to be
// live. A live stream's duration changes many times a second; announcing
// each change would exceed the room's command budget. It returns false when
// the event is about other media, which must be identified afresh.
func (s *Service) mediaUpdateLocked(gen uint64, playerSource string, title string, duration float64) (*protocol.Media, bool) {
	last := s.published
	if last.gen != gen || last.media == nil || playerSource == "" || playerSource != s.currentPlayerSource {
		return nil, false
	}
	media := *last.media
	if media.Title == "" {
		media.Title = title
	}
	switch {
	case s.currentLive:
		media.Live = true
	case duration > 0 && (media.DurationSeconds <= 0 || math.Abs(duration-media.DurationSeconds) >= 1):
		media.DurationSeconds = duration
	}
	return &media, true
}

// playbackRecovery bounds how often a failing network source is reloaded.
type playbackRecovery struct {
	itemID   string
	attempts int
	at       time.Time
}

const maxPlaybackRecoveries = 3

// recoverFailedPlayback reloads the selected queue item when its network
// source failed after it had loaded, such as a YouTube stream whose signed
// address expired or a broadcast that dropped out. It tries a few times,
// waiting longer each time, and only while the item stays selected; the
// reload follows the room like any other load.
func (s *Service) recoverFailedPlayback(client *faroclient.Client) {
	item, ok := selectedPlaylistItem(client.Snapshot().Playlist)
	s.mu.Lock()
	if !ok || item.URL == "" || s.currentSource != item.URL || s.client != client || s.sessionCtx == nil {
		s.mu.Unlock()
		return
	}
	if s.recovery.itemID != item.ID || time.Since(s.recovery.at) > 10*time.Minute {
		s.recovery = playbackRecovery{itemID: item.ID}
	}
	if s.recovery.attempts >= maxPlaybackRecoveries {
		s.mu.Unlock()
		return
	}
	s.recovery.attempts++
	s.recovery.at = time.Now()
	delay := playbackRecoveryDelay * time.Duration(math.Pow(3, float64(s.recovery.attempts-1)))
	ctx := s.sessionCtx
	s.mu.Unlock()
	s.log("player: the source failed; reloading it in %s", delay)
	time.AfterFunc(delay, func() {
		if latest, ok := selectedPlaylistItem(client.Snapshot().Playlist); !ok || latest.ID != item.ID {
			return
		}
		// The item is still marked as loaded if the player reported nothing
		// after the failure; it must load again.
		s.mu.Lock()
		if s.selectedItem == item.ID {
			s.selectedItem, s.selectedItemIdentity = "", ""
		}
		s.mu.Unlock()
		s.startSelectedPlaylist(ctx, client)
	})
}

// liveChatReader follows the chat of one live broadcast.
type liveChatReader struct {
	source  string
	session uint64
	cancel  context.CancelFunc
}

type liveChatSource interface {
	LiveChat(context.Context, string, func(youtube.ChatUpdate)) error
}

// followLiveChatLocked starts following the chat of the live YouTube
// broadcast at source, stopping the chat of anything else. A reload of the
// same broadcast, such as a quality change, keeps its chat.
func (s *Service) followLiveChatLocked(source string) {
	if s.liveChat != nil && s.liveChat.source == source {
		return
	}
	s.stopLiveChatLocked()
	chat, ok := s.youtube.(liveChatSource)
	if !ok || s.sessionCtx == nil {
		return
	}
	s.liveChatSession++
	ctx, cancel := context.WithCancel(s.sessionCtx)
	reader := &liveChatReader{source: source, session: s.liveChatSession, cancel: cancel}
	s.liveChat = reader
	go s.readLiveChat(ctx, chat, reader)
}

// stopLiveChatLocked stops following a broadcast's chat; the page hides it.
func (s *Service) stopLiveChatLocked() {
	if s.liveChat == nil {
		return
	}
	s.liveChat.cancel()
	session := s.liveChat.session
	s.liveChat = nil
	go s.sink(Event{Kind: "streamChat", StreamChat: &StreamChat{Session: session, State: "ended"}})
}

func (s *Service) readLiveChat(ctx context.Context, chat liveChatSource, reader *liveChatReader) {
	s.sink(Event{Kind: "streamChat", StreamChat: &StreamChat{Session: reader.session, State: "active"}})
	err := chat.LiveChat(ctx, reader.source, func(update youtube.ChatUpdate) {
		if ctx.Err() == nil {
			s.sink(Event{Kind: "streamChat", StreamChat: &StreamChat{
				Session: reader.session, State: "active", Messages: update.Messages, Removed: update.Removed,
			}})
		}
	})
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if s.liveChat == reader {
		s.liveChat = nil
	}
	s.mu.Unlock()
	status := &StreamChat{Session: reader.session, State: "ended"}
	if err != nil && !errors.Is(err, context.Canceled) {
		s.log("live chat: %v", err)
		status.State, status.Message = "unavailable", err.Error()
	}
	s.sink(Event{Kind: "streamChat", StreamChat: status})
}
