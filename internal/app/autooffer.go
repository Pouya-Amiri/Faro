package app

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

// autoOfferStagger is how long each participant who has the selected file
// waits behind the one before it. Every client orders the holders the same
// way, so normally only the first holder shares; the next one steps in only
// if nobody has shared by its turn (for example because the first holder
// turned automatic sharing off).
const autoOfferStagger = 3 * time.Second

// SetAutoOfferEnabled controls whether this participant automatically shares
// the selected queue file with people who do not have it.
func (s *Service) SetAutoOfferEnabled(enabled bool) {
	s.mu.Lock()
	s.autoOffer = enabled
	s.mu.Unlock()
	if client, err := s.connected(); err == nil {
		s.scheduleAutoOffer(s.root, client)
	}
}

type autoOfferPlan struct {
	itemID string
	path   string
	delay  time.Duration
}

// planAutoOffer decides whether this participant should share the selected
// file and after what delay. It performs no network I/O.
func (s *Service) planAutoOffer(client *faroclient.Client) (autoOfferPlan, bool) {
	s.mu.RLock()
	enabled, current := s.autoOffer, s.client == client
	manualOffer := s.streamPublisher != nil && !s.streamOfferAuto
	s.mu.RUnlock()
	if !enabled || !current || manualOffer || !client.Supports(protocol.CapabilityMediaStreamV1) || client.Welcome().Streaming == nil {
		return autoOfferPlan{}, false
	}
	snapshot := client.Snapshot()
	if snapshot.Playlist.Selected < 0 || snapshot.Playlist.Selected >= len(snapshot.Playlist.Items) {
		return autoOfferPlan{}, false
	}
	item := snapshot.Playlist.Items[snapshot.Playlist.Selected]
	if item.URL != "" || item.Media == nil || !strings.HasPrefix(item.Media.Fingerprint, "file-v1:") {
		return autoOfferPlan{}, false
	}
	s.mu.RLock()
	suppressed := s.autoOfferSuppressedItem == item.ID
	s.mu.RUnlock()
	if suppressed {
		return autoOfferPlan{}, false
	}
	fingerprint := item.Media.Fingerprint
	for _, offer := range snapshot.StreamOffers {
		if offer.Media.Fingerprint == fingerprint {
			return autoOfferPlan{}, false
		}
	}
	s.mu.RLock()
	path := s.sources[fingerprint]
	s.mu.RUnlock()
	if info, err := os.Stat(path); path == "" || err != nil || !info.Mode().IsRegular() {
		return autoOfferPlan{}, false
	}
	holders := []string{snapshot.SelfID}
	missing := false
	for _, participant := range snapshot.Participants {
		if participant.ID == snapshot.SelfID {
			continue
		}
		if slices.Contains(participant.AvailableMedia, fingerprint) {
			holders = append(holders, participant.ID)
		} else {
			missing = true
		}
	}
	if !missing {
		return autoOfferPlan{}, false
	}
	slices.Sort(holders)
	rank := slices.Index(holders, snapshot.SelfID)
	return autoOfferPlan{itemID: item.ID, path: path, delay: time.Duration(rank) * autoOfferStagger}, true
}

// scheduleAutoOffer withdraws an automatic offer that no longer matches the
// selection and (re)arms the timer that shares the selected file.
func (s *Service) scheduleAutoOffer(ctx context.Context, client *faroclient.Client) {
	s.mu.RLock()
	staleAuto := s.streamOfferAuto && s.streamOfferID != "" && s.client == client
	offeredItem := s.streamOfferItemID
	s.mu.RUnlock()
	if staleAuto {
		selected := client.Snapshot().Playlist
		if selected.Selected < 0 || selected.Selected >= len(selected.Items) || selected.Items[selected.Selected].ID != offeredItem {
			go func() {
				s.streamOfferMu.Lock()
				defer s.streamOfferMu.Unlock()
				s.mu.RLock()
				still := s.streamOfferAuto && s.streamOfferItemID == offeredItem && s.client == client
				s.mu.RUnlock()
				if still {
					_ = s.stopOfferingStream(client)
				}
			}()
		}
	}

	// An explicit "stop sharing" only holds for the item it was pressed on.
	playlist := client.Snapshot().Playlist
	selectedID := ""
	if playlist.Selected >= 0 && playlist.Selected < len(playlist.Items) {
		selectedID = playlist.Items[playlist.Selected].ID
	}
	s.mu.Lock()
	if s.autoOfferSuppressedItem != "" && s.autoOfferSuppressedItem != selectedID {
		s.autoOfferSuppressedItem = ""
	}
	s.mu.Unlock()

	plan, ok := s.planAutoOffer(client)
	key := plan.itemID + "|" + plan.path
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		if s.autoOfferTimer != nil {
			s.autoOfferTimer.Stop()
			s.autoOfferTimer, s.autoOfferKey = nil, ""
		}
		return
	}
	if s.autoOfferTimer != nil && s.autoOfferKey == key {
		return
	}
	if s.autoOfferTimer != nil {
		s.autoOfferTimer.Stop()
	}
	s.autoOfferKey = key
	s.autoOfferTimer = time.AfterFunc(plan.delay, func() { s.runAutoOffer(ctx, client, key) })
}

func (s *Service) runAutoOffer(ctx context.Context, client *faroclient.Client, key string) {
	s.mu.Lock()
	if s.autoOfferKey == key {
		s.autoOfferTimer, s.autoOfferKey = nil, ""
	}
	s.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	// Conditions may have changed while waiting: someone else shared it, the
	// selection moved, or the missing participant left.
	plan, ok := s.planAutoOffer(client)
	if !ok || plan.itemID+"|"+plan.path != key {
		return
	}
	s.streamOfferMu.Lock()
	defer s.streamOfferMu.Unlock()
	// Re-check under the offer lock: an explicit share may have won the race.
	if plan, ok = s.planAutoOffer(client); !ok || plan.itemID+"|"+plan.path != key {
		return
	}
	s.mu.RLock()
	replaceAuto := s.streamOfferAuto && s.streamOfferID != ""
	s.mu.RUnlock()
	if replaceAuto {
		_ = s.stopOfferingStream(client)
	}
	if err := s.offerStreamLocked(plan.path, 0, plan.itemID, true); err != nil {
		s.sink(Event{Kind: "error", Error: &protocol.Error{Code: "stream_auto_offer", Message: "Could not share the file automatically: " + err.Error()}})
		return
	}
	s.emitSnapshot()
}

// awaitingSharedCopy reports whether a participant who has the selected file
// is sharing it, or will share it automatically, so a missing local copy is
// not an error for this viewer.
func awaitingSharedCopy(snapshot protocol.Snapshot, fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	for _, offer := range snapshot.StreamOffers {
		if offer.Media.Fingerprint == fingerprint && offer.ProviderID != snapshot.SelfID {
			return true
		}
	}
	for _, participant := range snapshot.Participants {
		if participant.ID != snapshot.SelfID && slices.Contains(participant.AvailableMedia, fingerprint) {
			return true
		}
	}
	return false
}
