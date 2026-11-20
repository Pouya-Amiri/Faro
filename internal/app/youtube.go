package app

import (
	"context"
	"errors"
	"time"

	faroclient "github.com/Pouya-Amiri/Faro/internal/client"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
)

// youtubeDetailsTimeout bounds filling in a queued link's details.
const youtubeDetailsTimeout = 2 * time.Minute

// errQueueUnchanged ends a queue edit that has nothing to change.
var errQueueUnchanged = errors.New("queue unchanged")

// YouTubeTitle returns a video's title quickly, without waiting for yt-dlp,
// so a link can be queued at once. It fails for links that have no quick
// title, such as a channel's /live address, which then need YouTubeInfo.
func (s *Service) YouTubeTitle(source string) (string, error) {
	titles, supported := s.youtube.(interface {
		Title(context.Context, string) (string, error)
	})
	if !supported {
		return "", errors.New("quick YouTube titles are not available")
	}
	return titles.Title(s.root, source)
}

type youtubeAccounts interface {
	SetAccount(youtube.Account) error
	Account() youtube.Account
	CheckAccount(context.Context) (youtube.AccountStatus, error)
}

// SetYouTubeAccount chooses the browser or cookies file whose YouTube sign-in
// yt-dlp uses. The account never leaves this device: yt-dlp sends its
// cookies only to YouTube.
func (s *Service) SetYouTubeAccount(account youtube.Account) (youtube.Account, error) {
	accounts, supported := s.youtube.(youtubeAccounts)
	if !supported {
		return youtube.Account{}, errors.New("YouTube sign-in is not available")
	}
	if err := accounts.SetAccount(account); err != nil {
		return youtube.Account{}, err
	}
	return accounts.Account(), nil
}

// CheckYouTubeAccount reports whether yt-dlp finds a YouTube sign-in where
// the account says.
func (s *Service) CheckYouTubeAccount() (youtube.AccountStatus, error) {
	accounts, supported := s.youtube.(youtubeAccounts)
	if !supported {
		return youtube.AccountStatus{}, errors.New("YouTube sign-in is not available")
	}
	return accounts.CheckAccount(s.root)
}

// fillYouTubeDetails adds the duration to YouTube links that were queued
// before yt-dlp had read it. The page queues a link as soon as its title is
// known; the rest of its details follow here. The duration is set only on
// the item this participant added, and only while it is still unknown.
func (s *Service) fillYouTubeDetails(client *faroclient.Client, items []protocol.PlaylistItem) {
	for _, item := range items {
		if item.URL == "" || !youtube.IsURL(item.URL) || item.Media == nil || item.Media.DurationSeconds > 0 {
			continue
		}
		go s.fillYouTubeDuration(client, item.ID, item.URL)
	}
}

func (s *Service) fillYouTubeDuration(client *faroclient.Client, itemID, source string) {
	ctx, cancel := context.WithTimeout(s.root, youtubeDetailsTimeout)
	defer cancel()
	// Leaving the room ends the wait; another room has another queue.
	go func() {
		select {
		case <-client.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	info, err := s.youtube.Inspect(ctx, source)
	if err != nil || info.Live || info.Duration <= 0 || ctx.Err() != nil {
		return
	}
	err = s.editPlaylist(client, func(items []protocol.PlaylistItem) ([]protocol.PlaylistItem, error) {
		for index := range items {
			item := &items[index]
			if item.ID != itemID {
				continue
			}
			if item.URL != source || item.Media == nil || item.Media.DurationSeconds > 0 {
				return nil, errQueueUnchanged
			}
			item.Media.DurationSeconds = info.Duration
			return items, nil
		}
		return nil, errQueueUnchanged
	})
	if err != nil && !errors.Is(err, errQueueUnchanged) {
		s.log("youtube: could not add the duration of a queued link: %v", err)
	}
}
