//go:build !windows && !darwin

package discovery

import (
	"os"
	"path/filepath"
)

func platformCandidates(profile string) []string {
	home, _ := os.UserHomeDir()
	var candidates []string
	directories := []string{
		"/usr/local/bin", "/usr/bin", "/snap/bin",
		filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"),
		filepath.Join(home, ".nix-profile", "bin"),
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	directories = append(directories, filepath.Join(stateHome, "nix", "profile", "bin"))
	for _, name := range platformExecutableNames(profile) {
		for _, directory := range directories {
			candidates = append(candidates, filepath.Join(directory, name))
		}
	}
	appImageNames := map[string][]string{
		"mpv":     {"*[Mm][Pp][Vv]*.AppImage"},
		"memento": {"*[Mm]emento*.AppImage"}, "vlc": {"*[Vv][Ll][Cc]*.AppImage"},
	}[profile]
	for _, name := range appImageNames {
		candidates = append(candidates, filepath.Join(home, "Applications", name), filepath.Join(home, ".local", "Applications", name))
	}
	return candidates
}

func platformSupports(profile string) bool {
	return profile == "mpv" || profile == "memento" || profile == "vlc"
}

func platformExecutableNames(profile string) []string {
	return map[string][]string{
		"mpv": {"mpv"}, "memento": {"memento"}, "vlc": {"vlc", "cvlc"},
	}[profile]
}
