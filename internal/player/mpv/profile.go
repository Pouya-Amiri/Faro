package mpv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Pouya-Amiri/Faro/internal/player/discovery"
)

type Profile string

const (
	ProfileMPV     Profile = "mpv"
	ProfileMPVNet  Profile = "mpv.net"
	ProfileIINA    Profile = "iina"
	ProfileMemento Profile = "memento"
)

type Config struct {
	Profile    Profile
	Executable string
	ExtraArgs  []string
}

func (c Config) executable() (string, error) {
	if c.Executable != "" {
		return preferWindowsExecutable(c.Profile, c.Executable), nil
	}
	return discovery.Find(string(c.Profile))
}

func preferWindowsExecutable(profile Profile, executable string) string {
	if runtime.GOOS != "windows" || profile != ProfileMPV && profile != ProfileMPVNet {
		return executable
	}
	if filepath.Ext(executable) == "" {
		if resolved, err := exec.LookPath(executable + ".exe"); err == nil {
			return resolved
		}
		return executable
	}
	if !strings.EqualFold(filepath.Ext(executable), ".com") {
		return executable
	}
	sibling := strings.TrimSuffix(executable, filepath.Ext(executable)) + ".exe"
	if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
		return sibling
	}
	return executable
}

func (c Config) arguments(ipcPath, initialSource string) []string {
	// User options come first so Faro's lifecycle and IPC requirements cannot
	// be accidentally overridden by saved Extra Arguments.
	args := safeExtraArguments(c.ExtraArgs)
	if c.Profile == ProfileIINA {
		args = append(args,
			// iina-cli otherwise exits immediately after launching the app. Faro
			// owns this dedicated instance and needs the wrapper to track it.
			"--keep-running",
			"--no-stdin",
			"--mpv-input-ipc-server="+ipcPath,
		)
	} else {
		args = append(args,
			"--idle=yes",
			"--force-window=yes",
			"--input-ipc-server="+ipcPath,
			"--no-terminal",
		)
		if c.Profile == ProfileMPVNet {
			args = append(args,
				// mpv.net defaults to one shared process. A dedicated process is
				// required so Faro owns the unique IPC pipe it created above.
				"--process-instance=multi",
				"--auto-load-folder=no",
			)
		}
	}
	if initialSource != "" {
		args = append(args, initialSource)
	}
	return args
}

func safeExtraArguments(values []string) []string {
	result := make([]string, 0, len(values))
	for index := 0; index < len(values); index++ {
		value := values[index]
		key := strings.ToLower(strings.SplitN(value, "=", 2)[0])
		switch key {
		case "--input-ipc-server", "--mpv-input-ipc-server", "--process-instance", "--idle":
			// The space-separated forms consume the next argument as their value.
			if value == key && index+1 < len(values) {
				index++
			}
			continue
		case "--no-idle":
			continue
		}
		result = append(result, value)
	}
	return result
}
