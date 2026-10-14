//go:build darwin

package discovery

import (
	"os"
	"path/filepath"
)

func platformCandidates(profile string) []string {
	home, _ := os.UserHomeDir()
	var candidates []string
	for _, launcher := range bundleLaunchers(profile) {
		app := filepath.Join(bundleNames[profile], launcher)
		candidates = append(candidates, filepath.Join("/Applications", app), filepath.Join(home, "Applications", app))
	}
	for _, prefix := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin", filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
		for _, name := range platformExecutableNames(profile) {
			candidates = append(candidates, filepath.Join(prefix, name))
		}
	}
	return candidates
}

var bundleNames = map[string]string{"mpv": "mpv.app", "iina": "IINA.app", "memento": "Memento.app", "vlc": "VLC.app"}

// bundleLaunchers returns the launcher's path inside the player's app bundle.
func bundleLaunchers(profile string) []string {
	return map[string][]string{
		"mpv":     {"Contents/MacOS/mpv"},
		"iina":    {"Contents/MacOS/iina-cli"},
		"memento": {"Contents/MacOS/Memento", "Contents/MacOS/memento"},
		"vlc":     {"Contents/MacOS/VLC"},
	}[profile]
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
