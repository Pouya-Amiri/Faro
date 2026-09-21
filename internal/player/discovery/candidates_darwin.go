//go:build darwin

package discovery

import (
	"os"
	"path/filepath"
)

func platformCandidates(profile string) []string {
	home, _ := os.UserHomeDir()
	relative := map[string][]string{
		"mpv":     {"mpv.app/Contents/MacOS/mpv"},
		"mpv.net": {"mpv.net.app/Contents/MacOS/mpvnet"},
		"iina":    {"IINA.app/Contents/MacOS/iina-cli"},
		"memento": {"Memento.app/Contents/MacOS/Memento", "Memento.app/Contents/MacOS/memento"},
		"vlc":     {"VLC.app/Contents/MacOS/VLC"},
	}[profile]
	var candidates []string
	for _, app := range relative {
		candidates = append(candidates, filepath.Join("/Applications", app), filepath.Join(home, "Applications", app))
	}
	name := map[string]string{"mpv": "mpv", "mpv.net": "mpvnet", "iina": "iina-cli", "memento": "memento", "vlc": "vlc"}[profile]
	for _, prefix := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin", filepath.Join(home, ".local", "bin")} {
		candidates = append(candidates, filepath.Join(prefix, name))
	}
	return candidates
}
