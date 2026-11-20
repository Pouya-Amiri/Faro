package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/Pouya-Amiri/Faro/internal/youtube"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// youtubeAccountMu serialises reading and writing the saved account.
var youtubeAccountMu sync.Mutex

func youtubeAccountPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "faro", "youtube.json"), nil
}

// loadYouTubeAccount reads the saved choice of sign-in. It holds where the
// sign-in comes from (a browser name or a file path), never a cookie.
func loadYouTubeAccount() youtube.Account {
	var account youtube.Account
	path, err := youtubeAccountPath()
	if err != nil {
		return account
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &account) != nil {
		return youtube.Account{}
	}
	return account
}

func saveYouTubeAccount(account youtube.Account) error {
	path, err := youtubeAccountPath()
	if err != nil {
		return err
	}
	if !account.Enabled() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(account)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// restoreYouTubeAccount applies the saved sign-in at startup. A browser that
// was uninstalled or a file that was moved since is reported in the log and
// left for the Preferences page to show.
func (d *Desktop) restoreYouTubeAccount() {
	youtubeAccountMu.Lock()
	account := loadYouTubeAccount()
	youtubeAccountMu.Unlock()
	if !account.Enabled() {
		return
	}
	if _, err := d.service.SetYouTubeAccount(account); err != nil {
		d.diagnostics.Printf("youtube: the saved sign-in cannot be used: %v", err)
	}
}

// YouTubeAccount returns the saved choice of YouTube sign-in.
func (d *Desktop) YouTubeAccount() youtube.Account {
	youtubeAccountMu.Lock()
	defer youtubeAccountMu.Unlock()
	return loadYouTubeAccount()
}

// SetYouTubeAccount validates, applies and saves the choice of YouTube
// sign-in, returning it as saved.
func (d *Desktop) SetYouTubeAccount(account youtube.Account) (youtube.Account, error) {
	youtubeAccountMu.Lock()
	defer youtubeAccountMu.Unlock()
	applied, err := d.service.SetYouTubeAccount(account)
	if err != nil {
		return youtube.Account{}, err
	}
	if err := saveYouTubeAccount(applied); err != nil {
		return youtube.Account{}, err
	}
	return applied, nil
}

// CheckYouTubeAccount reports whether yt-dlp finds a YouTube sign-in in the
// chosen browser or file, without contacting YouTube.
func (d *Desktop) CheckYouTubeAccount() (youtube.AccountStatus, error) {
	return d.service.CheckYouTubeAccount()
}

// ChooseCookiesFile asks for a cookies.txt file exported from a browser.
func (d *Desktop) ChooseCookiesFile() (string, error) {
	return d.app.Dialog.OpenFileWithOptions(&application.OpenFileDialogOptions{
		Title:  "Choose a cookies.txt file",
		Window: d.window,
		Filters: []application.FileFilter{
			{DisplayName: "Cookies files", Pattern: "*.txt"},
			{DisplayName: "All files", Pattern: "*"},
		},
	}).PromptForSingleSelection()
}

// YouTubeTitle returns a video's title without waiting for yt-dlp.
func (d *Desktop) YouTubeTitle(source string) (string, error) { return d.service.YouTubeTitle(source) }
