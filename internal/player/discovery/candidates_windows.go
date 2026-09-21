//go:build windows

package discovery

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func platformCandidates(profile string) []string {
	names := platformExecutableNames(profile)
	var candidates []string
	for _, name := range names {
		candidates = append(candidates, appPathCandidates(name)...)
	}
	programDirs := []string{os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")}
	installDirs := map[string][]string{
		"mpv": {"mpv"}, "mpv.net": {"mpv.net"}, "memento": {"Memento"},
		"vlc": {filepath.Join("VideoLAN", "VLC")},
	}[profile]
	for _, root := range programDirs {
		if root == "" {
			continue
		}
		for _, directory := range installDirs {
			for _, name := range names {
				candidates = append(candidates, filepath.Join(root, directory, name))
			}
		}
	}
	local, user, chocolatey := os.Getenv("LOCALAPPDATA"), os.Getenv("USERPROFILE"), chocolateyInstall()
	for _, name := range names {
		if local != "" {
			candidates = append(candidates,
				filepath.Join(local, "Programs", profile, name),
				filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", name),
				filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", "*", name),
				filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", "*", "*", name),
				filepath.Join(local, "Microsoft", "WindowsApps", name))
		}
		if user != "" {
			candidates = append(candidates,
				filepath.Join(user, "scoop", "apps", profile, "current", name),
				filepath.Join(user, "scoop", "apps", "*", "current", name))
		}
		candidates = append(candidates,
			filepath.Join(chocolatey, "bin", name),
			filepath.Join(chocolatey, "lib", "*", "tools", name),
			filepath.Join(chocolatey, "lib", "*", "tools", "*", name),
		)
	}
	return candidates
}

func platformSupports(profile string) bool {
	return profile == "mpv" || profile == "mpv.net" || profile == "memento" || profile == "vlc"
}

func platformExecutableNames(profile string) []string {
	return map[string][]string{
		"mpv": {"mpv.exe", "mpv.com", "mpv"},
		// Both launchers are present in the official mpv.net portable archive;
		// prefer the GUI executable while retaining the console launcher fallback.
		"mpv.net": {"mpvnet.exe", "mpvnet.com", "mpvnet"},
		"memento": {"memento.exe", "memento"}, "vlc": {"vlc.exe", "vlc"},
	}[profile]
}

func chocolateyInstall() string {
	if configured := os.Getenv("ChocolateyInstall"); configured != "" {
		return configured
	}
	programData := os.Getenv("ProgramData")
	if programData == "" {
		drive := strings.TrimRight(os.Getenv("SystemDrive"), `\\/`)
		if drive == "" {
			drive = "C:"
		}
		programData = drive + `\ProgramData`
	}
	return filepath.Join(programData, "chocolatey")
}

func appPathCandidates(executable string) []string {
	keyPath := `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\` + executable
	var candidates []string
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		for _, access := range []uint32{registry.QUERY_VALUE | registry.WOW64_64KEY, registry.QUERY_VALUE | registry.WOW64_32KEY} {
			key, err := registry.OpenKey(root, keyPath, access)
			if err != nil {
				continue
			}
			value, _, err := key.GetStringValue("")
			_ = key.Close()
			if err == nil {
				candidates = append(candidates, value)
			}
		}
	}
	return candidates
}
