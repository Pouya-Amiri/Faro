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
		"iina":    {"IINA.app/Contents/MacOS/iina-cli"},
		"memento": {"Memento.app/Contents/MacOS/Memento", "Memento.app/Contents/MacOS/memento"},
		"vlc":     {"VLC.app/Contents/MacOS/VLC"},
	}[profile]
	var candidates []string
	for _, app := range relative {
		candidates = append(candidates, filepath.Join("/Applications", app), filepath.Join(home, "Applications", app))
	}
	for _, prefix := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin", filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
		for _, name := range platformExecutableNames(profile) {
			candidates = append(candidates, filepath.Join(prefix, name))
		}
	}
	return candidates
}

func platformSupports(profile string) bool {
	return profile == "mpv" || profile == "iina" || profile == "memento" || profile == "vlc"
}

func platformExecutableNames(profile string) []string {
	return map[string][]string{
		"mpv": {"mpv"}, "iina": {"iina", "iina-cli"},
		"memento": {"memento"}, "vlc": {"vlc"},
	}[profile]
}
