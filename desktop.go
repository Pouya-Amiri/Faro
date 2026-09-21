package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Pouya-Amiri/Faro/internal/app"
	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
	"github.com/Pouya-Amiri/Faro/internal/legal"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
	"github.com/Pouya-Amiri/Faro/internal/youtube"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type Desktop struct {
	app     *application.App
	window  application.Window
	service *app.Service
}

func NewDesktop(wailsApp *application.App) *Desktop { return &Desktop{app: wailsApp} }

func (d *Desktop) setWindow(window application.Window) { d.window = window }

func (d *Desktop) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	d.service = app.New(ctx, func(event app.Event) {
		d.app.Event.Emit("faro:event", event)
	})
	return nil
}

func (d *Desktop) ServiceShutdown() error {
	if d.service != nil {
		d.service.Shutdown()
	}
	return nil
}

func (d *Desktop) Version() string { return buildinfo.EffectiveVersion() }

func (d *Desktop) LegalInfo() legal.Information {
	return legal.Info(buildinfo.EffectiveVersion())
}

func (d *Desktop) DetectPlayer(player string) (string, error) { return app.DetectPlayer(player) }

func (d *Desktop) SupportedPlayers() []string { return app.SupportedPlayers() }

func (d *Desktop) Connect(request app.ConnectionRequest) error {
	request.ClientVersion = buildinfo.EffectiveVersion()
	return d.service.Connect(request)
}

func (d *Desktop) Disconnect() { d.service.Disconnect() }

func (d *Desktop) Snapshot() (protocol.Snapshot, error) { return d.service.Snapshot() }

func (d *Desktop) SendChat(message string) error { return d.service.SendChat(message) }

func (d *Desktop) SetRoomMode(mode protocol.RoomMode) error { return d.service.SetRoomMode(mode) }

func (d *Desktop) SetRole(participantID string, role protocol.Role) error {
	return d.service.SetRole(participantID, role)
}

func (d *Desktop) SetPaused(paused bool) error { return d.service.SetPaused(paused) }

func (d *Desktop) Seek(seconds float64) error { return d.service.Seek(seconds) }

func (d *Desktop) SetRate(rate float64) error { return d.service.SetRate(rate) }

func (d *Desktop) MoveRoom(room string) error { return d.service.MoveRoom(room) }

func (d *Desktop) StopOfferingStream() error { return d.service.StopOfferingStream() }

func (d *Desktop) StreamingAvailable() bool { return d.service.StreamingAvailable() }

func (d *Desktop) YouTubeInfo(source string) (youtube.Info, error) {
	return d.service.YouTubeInfo(source)
}

func (d *Desktop) SetYouTubeQuality(source string, height int) error {
	return d.service.SetYouTubeQuality(source, height)
}

func (d *Desktop) ChangeYouTubeQuality(source string, height int) error {
	return d.service.ChangeYouTubeQuality(source, height)
}

func (d *Desktop) SetSponsorBlockEnabled(enabled bool) {
	d.service.SetSponsorBlockEnabled(enabled)
}

func (d *Desktop) TimelineSegments() []app.TimelineSegment {
	return d.service.TimelineSegments()
}

func (d *Desktop) SetPlaylist(items []app.PlaylistInput) error {
	return d.service.SetPlaylist(items)
}

func (d *Desktop) SelectPlaylist(index int) error { return d.service.SelectPlaylist(index) }

func (d *Desktop) SpinPlaylistWheel() error { return d.service.SpinPlaylistWheel() }

func (d *Desktop) LocatePlaylistItem(itemID, source string) error {
	return d.service.LocatePlaylistItem(itemID, source)
}

func (d *Desktop) OfferPlaylistStream(itemID string) error {
	return d.service.OfferPlaylistStream(itemID)
}

func (d *Desktop) PlaylistAvailability() map[string]bool { return d.service.PlaylistAvailability() }

func (d *Desktop) IndexMediaDirectory(directory string) (int, error) {
	return d.service.IndexMediaDirectory(directory)
}

func (d *Desktop) OwnerInvite() (string, error) { return d.service.OwnerInvite() }

func (d *Desktop) ParticipantInvite() (string, error) { return d.service.ParticipantInvite() }

func (d *Desktop) StartServer(request app.ServerRequest) (app.LocalServerStatus, error) {
	return d.service.StartServer(request)
}

func (d *Desktop) ServerStatus() app.LocalServerStatus { return d.service.ServerStatus() }

func (d *Desktop) StopServer() { d.service.StopServer() }

func (d *Desktop) ChooseMediaFile() (string, error) {
	return d.app.Dialog.OpenFileWithOptions(&application.OpenFileDialogOptions{
		Title:   "Choose media",
		Window:  d.window,
		Filters: mediaFileFilters(),
	}).PromptForSingleSelection()
}

func (d *Desktop) ChooseMediaFiles() ([]string, error) {
	return d.app.Dialog.OpenFileWithOptions(&application.OpenFileDialogOptions{
		Title:   "Choose media",
		Window:  d.window,
		Filters: mediaFileFilters(),
	}).PromptForMultipleSelection()
}

func (d *Desktop) ExpandMediaPaths(paths []string) ([]string, error) {
	return app.ExpandMediaPaths(paths, 500)
}

func (d *Desktop) ChooseMediaDirectory() (string, error) {
	return d.app.Dialog.OpenFileWithOptions(&application.OpenFileDialogOptions{
		Title:                "Choose media directory",
		Window:               d.window,
		CanChooseDirectories: true,
		CanChooseFiles:       false,
	}).PromptForSingleSelection()
}

func (d *Desktop) LoadPlaylistFile() ([]string, error) {
	path, err := d.app.Dialog.OpenFileWithOptions(&application.OpenFileDialogOptions{
		Title:  "Load playlist",
		Window: d.window,
		Filters: []application.FileFilter{
			{DisplayName: "Playlist files", Pattern: "*.m3u;*.m3u8;*.txt"},
			{DisplayName: "All files", Pattern: "*"},
		},
	}).PromptForSingleSelection()
	if err != nil || path == "" {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 1024*1024 {
		return nil, errors.New("playlist file exceeds the 1 MiB limit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0)
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\ufeff"), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "://") && !filepath.IsAbs(line) {
			line = filepath.Join(filepath.Dir(path), line)
		}
		lines = append(lines, line)
		if len(lines) > 500 {
			return nil, errors.New("playlist file exceeds the 500-item limit")
		}
	}
	return lines, nil
}

func (d *Desktop) SavePlaylistFile() (string, error) {
	lines, err := d.service.PlaylistExportLines()
	if err != nil {
		return "", err
	}
	path, err := d.app.Dialog.SaveFileWithOptions(&application.SaveFileDialogOptions{
		Title:    "Save playlist",
		Window:   d.window,
		Filename: "faro-playlist.m3u8",
		Filters: []application.FileFilter{
			{DisplayName: "M3U8 playlist", Pattern: "*.m3u8"},
			{DisplayName: "Text file", Pattern: "*.txt"},
		},
	}).PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	if err := os.WriteFile(path, []byte("#EXTM3U\n"+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func mediaFileFilters() []application.FileFilter {
	return []application.FileFilter{
		{DisplayName: "Media files", Pattern: "*.mkv;*.mp4;*.webm;*.avi;*.mov;*.m4v;*.mp3;*.m4a;*.flac;*.ogg;*.wav;*.opus"},
		{DisplayName: "All files", Pattern: "*"},
	}
}
