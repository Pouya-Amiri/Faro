//go:build windows

package discovery

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

func platformCandidates(profile string) []string {
	names := map[string][]string{
		"mpv": {"mpv.exe"}, "mpv.net": {"mpvnet.exe", "mpvnet.com"},
		"iina": {"iina-cli.exe"}, "memento": {"memento.exe"}, "vlc": {"vlc.exe"},
	}[profile]
	var candidates []string
	for _, name := range names {
		candidates = append(candidates, appPathCandidates(name)...)
	}
	programDirs := []string{os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")}
	installDirs := map[string][]string{
		"mpv": {"mpv"}, "mpv.net": {"mpv.net"}, "iina": {"IINA"}, "memento": {"Memento"},
		"vlc": {"VideoLAN", filepath.Join("VideoLAN", "VLC")},
	}[profile]
	for _, root := range programDirs {
		for _, directory := range installDirs {
			for _, name := range names {
				candidates = append(candidates, filepath.Join(root, directory, name))
			}
		}
	}
	local, user, chocolatey := os.Getenv("LOCALAPPDATA"), os.Getenv("USERPROFILE"), os.Getenv("ChocolateyInstall")
	for _, name := range names {
		candidates = append(candidates,
			filepath.Join(local, "Programs", profile, name),
			filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", name),
			filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", "*", name),
			filepath.Join(local, "Microsoft", "WinGet", "Packages", "*", "*", "*", name),
			filepath.Join(local, "Microsoft", "WindowsApps", name),
			filepath.Join(user, "scoop", "apps", profile, "current", name),
			filepath.Join(user, "scoop", "apps", "*", "current", name),
			filepath.Join(chocolatey, "bin", name),
			filepath.Join(chocolatey, "lib", "*", "tools", name),
			filepath.Join(chocolatey, "lib", "*", "tools", "*", name),
		)
	}
	return candidates
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
