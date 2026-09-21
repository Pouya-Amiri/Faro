// Package discovery locates supported media-player executables installed by
// native installers, package managers, app bundles, and portable layouts.
package discovery

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Find returns an absolute path to the selected player's launcher.
func Find(player string) (string, error) {
	profile, names, err := executableNames(player)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// PATHEXT checks .COM before .EXE for extensionless names. mpv.com is
		// only a console launcher which spawns mpv.exe, so prefer the real
		// executable and avoid creating a needlessly fragile process tree.
		names = preferWindowsExecutables(names)
	}
	for _, name := range names {
		if path, lookErr := exec.LookPath(name); lookErr == nil {
			return absolute(path), nil
		}
	}
	patterns := platformCandidates(profile)
	if application, applicationErr := os.Executable(); applicationErr == nil {
		for _, name := range names {
			patterns = append(patterns, filepath.Join(filepath.Dir(application), name))
		}
	}
	for _, candidate := range expandCandidates(patterns) {
		if usableFile(candidate) {
			return absolute(candidate), nil
		}
	}
	return "", fmt.Errorf("%s executable not found; install it or choose its location manually", displayName(profile))
}

func preferWindowsExecutables(names []string) []string {
	result := make([]string, 0, len(names))
	for _, suffix := range []string{".exe", ".com", ""} {
		for _, name := range names {
			extension := strings.ToLower(filepath.Ext(name))
			if extension == suffix || suffix == "" && extension == "" {
				result = append(result, name)
			}
		}
	}
	return result
}

func executableNames(player string) (string, []string, error) {
	switch strings.ToLower(strings.TrimSpace(player)) {
	case "", "mpv":
		return "mpv", []string{"mpv", "mpv.exe"}, nil
	case "mpv.net", "mpvnet":
		return "mpv.net", []string{"mpvnet", "mpvnet.exe", "mpvnet.com"}, nil
	case "iina":
		return "iina", []string{"iina-cli"}, nil
	case "memento":
		return "memento", []string{"memento", "memento.exe"}, nil
	case "vlc":
		return "vlc", []string{"vlc", "cvlc", "vlc.exe"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported player %q", player)
	}
}

func displayName(profile string) string {
	switch profile {
	case "iina":
		return "IINA"
	case "vlc":
		return "VLC"
	case "memento":
		return "Memento"
	default:
		return profile
	}
}

func expandCandidates(patterns []string) []string {
	seen := make(map[string]struct{}, len(patterns))
	var result []string
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		matches := []string{pattern}
		if strings.ContainsAny(pattern, "*?[") {
			matches, _ = filepath.Glob(pattern)
		}
		for _, match := range matches {
			key := filepath.Clean(match)
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, match)
		}
	}
	return result
}

func usableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

func absolute(path string) string {
	result, err := filepath.Abs(path)
	if err == nil {
		return result
	}
	return path
}
