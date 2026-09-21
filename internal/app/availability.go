package app

import (
	"errors"
	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"slices"
)

func (s *Service) publishAvailability(client *faroclient.Client, available map[string]bool) {
	if !client.Supports(protocol.CapabilityMediaAvailabilityV1) {
		return
	}
	fingerprints := []string{}
	for _, item := range client.Snapshot().Playlist.Items {
		if available[item.ID] && item.Media != nil && item.Media.Fingerprint != "" {
			fingerprints = append(fingerprints, item.Media.Fingerprint)
		}
	}
	slices.Sort(fingerprints)
	fingerprints = slices.Compact(fingerprints)
	s.availabilityMu.Lock()
	defer s.availabilityMu.Unlock()
	if s.availabilityClient == client && slices.Equal(fingerprints, s.availabilitySent) {
		return
	}
	if err := client.SetMediaAvailability(protocol.MediaAvailabilitySet{Fingerprints: fingerprints}); err == nil {
		s.availabilityClient, s.availabilitySent = client, fingerprints
	}
}

func (s *Service) OfferPlaylistStream(itemID string) error {
	client, err := s.connected()
	if err != nil {
		return err
	}
	for _, item := range client.Snapshot().Playlist.Items {
		if item.ID != itemID || item.Media == nil {
			continue
		}
		s.mu.RLock()
		path := s.sources[item.Media.Fingerprint]
		s.mu.RUnlock()
		if path == "" {
			return errors.New("locate your copy before sharing it")
		}
		if err := s.offerStream(path, 0); err != nil {
			return err
		}
		s.mu.Lock()
		if s.streamOfferMedia != nil && s.streamOfferMedia.Fingerprint == item.Media.Fingerprint {
			s.streamOfferItemID = item.ID
		}
		s.mu.Unlock()
		return nil
	}
	return errors.New("playlist item is no longer available")
}
