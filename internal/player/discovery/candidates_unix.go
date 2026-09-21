//go:build !windows && !darwin

package discovery

import (
	"os"
	"path/filepath"
)

func platformCandidates(profile string) []string {
	home, _ := os.UserHomeDir()
	binNames := map[string][]string{
		"mpv":     {"mpv"},
		"mpv.net": {"mpvnet", "mpvnet.exe"},
		"iina":    {"iina-cli"},
		"memento": {"memento", "memento.exe"},
		"vlc":     {"vlc", "cvlc"},
	}[profile]
	var candidates []string
	for _, name := range binNames {
		for _, directory := range []string{"/usr/local/bin", "/usr/bin", "/snap/bin", filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
			candidates = append(candidates, filepath.Join(directory, name))
		}
	}
	flatpakID := map[string]string{"mpv": "io.mpv.Mpv", "vlc": "org.videolan.VLC"}[profile]
	flatpakBinary := map[string]string{"mpv": "mpv", "vlc": "vlc"}[profile]
	if flatpakID != "" {
		for _, root := range []string{filepath.Join(home, ".local", "share", "flatpak"), "/var/lib/flatpak"} {
			candidates = append(candidates,
				filepath.Join(root, "exports", "bin", flatpakID),
				filepath.Join(root, "app", flatpakID, "current", "active", "files", "bin", flatpakBinary),
				filepath.Join(root, "app", flatpakID, "*", "active", "files", "bin", flatpakBinary),
			)
		}
	}
	appImageNames := map[string][]string{
		"mpv": {"*[Mm][Pp][Vv]*.AppImage"}, "mpv.net": {"*[Mm][Pp][Vv]*[Nn][Ee][Tt]*.AppImage"},
		"memento": {"*[Mm]emento*.AppImage"}, "vlc": {"*[Vv][Ll][Cc]*.AppImage"},
	}[profile]
	for _, name := range appImageNames {
		candidates = append(candidates, filepath.Join(home, "Applications", name), filepath.Join(home, ".local", "Applications", name))
	}
	return candidates
}
