package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Method names how this copy of Faro was installed, which decides how an
// update is applied.
type Method string

const (
	MethodManual           Method = "manual"
	MethodWindowsInstaller Method = "windows-installer"
	MethodWindowsPortable  Method = "windows-portable"
	MethodMacApp           Method = "macos-app"
	MethodAppImage         Method = "appimage"
	MethodDeb              Method = "deb"
	MethodRPM              Method = "rpm"
)

// errCancelled reports that the user declined a system authentication
// prompt. It is not shown as a failure.
var errCancelled = errors.New("update cancelled")

// installation describes the running copy of Faro.
type installation struct {
	method Method
	// target is the file or bundle an update replaces.
	target string
	// reason explains, for MethodManual, why Faro cannot update itself.
	reason string
	// allUsers marks a Windows installation shared by every account.
	allUsers bool
}

// assetName is the release file that updates this installation.
func (i installation) assetName(release string) string {
	return i.assetNameFor(release, runtime.GOARCH)
}

func (i installation) assetNameFor(release, goarch string) string {
	switch i.method {
	case MethodWindowsInstaller:
		return fmt.Sprintf("Faro-%s-windows-%s-setup.exe", release, archName(goarch, "x86_64", "arm64"))
	case MethodWindowsPortable:
		return fmt.Sprintf("Faro-%s-windows-%s-portable.zip", release, archName(goarch, "x86_64", "arm64"))
	case MethodMacApp:
		return fmt.Sprintf("Faro-%s-macos-%s.dmg", release, archName(goarch, "x86_64", "arm64"))
	case MethodAppImage:
		return fmt.Sprintf("Faro-%s-linux-%s.AppImage", release, archName(goarch, "x86_64", "aarch64"))
	case MethodDeb:
		return fmt.Sprintf("Faro-%s-linux-%s.deb", release, goarch)
	case MethodRPM:
		return fmt.Sprintf("Faro-%s-linux-%s.rpm", release, archName(goarch, "x86_64", "aarch64"))
	}
	return ""
}

// note tells the user what installing will involve.
func (i installation) note() string {
	switch i.method {
	case MethodManual:
		return i.reason
	case MethodDeb, MethodRPM:
		return "Your system will ask for your password to install the package."
	case MethodWindowsInstaller:
		if i.allUsers {
			return "Windows will ask for permission, since Faro is installed for all users."
		}
	}
	return ""
}

func archName(goarch, amd64, arm64 string) string {
	switch goarch {
	case "amd64":
		return amd64
	case "arm64":
		return arm64
	}
	return goarch
}

// applyFunc installs the verified file and returns how to start the new
// version once this process has exited.
type applyFunc func(ctx context.Context, current installation, file, release string) (relaunch func() error, err error)

// waitEnvironment carries the PID of the Faro that started an updated copy.
// The new process waits for it to exit before it claims the single-instance
// lock, which the old process still holds while it shuts down.
const waitEnvironment = "FARO_UPDATED_FROM_PID"

// WaitForPredecessor blocks, briefly, until the Faro that relaunched this
// process after an update has exited. It must run before the app starts.
func WaitForPredecessor() {
	value := os.Getenv(waitEnvironment)
	if value == "" {
		return
	}
	_ = os.Unsetenv(waitEnvironment)
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return
	}
	waitForExit(pid, 30*time.Second)
}

// relaunchEnvironment is this process's environment for a relaunched copy,
// without variables that described this particular launch.
func relaunchEnvironment(drop ...string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		keep := name != waitEnvironment
		for _, dropped := range drop {
			keep = keep && name != dropped
		}
		if keep {
			environment = append(environment, entry)
		}
	}
	return append(environment, waitEnvironment+"="+strconv.Itoa(os.Getpid()))
}

// writableDirectory reports whether files can be created in directory.
func writableDirectory(directory string) bool {
	file, err := os.CreateTemp(directory, ".faro-write-test-")
	if err != nil {
		return false
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
	return true
}

// replaceFile atomically replaces target with the contents of source,
// keeping the new file's mode. The replacement is staged next to target so
// the final rename stays on one file system.
func replaceFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	return writeFileAtomic(target, input, mode)
}

func writeFileAtomic(target string, contents io.Reader, mode os.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".update-")
	if err != nil {
		return err
	}
	stagedName := staged.Name()
	defer os.Remove(stagedName)
	if _, err := io.Copy(staged, io.LimitReader(contents, maximumDownload)); err != nil {
		_ = staged.Close()
		return err
	}
	if err := staged.Chmod(mode); err != nil {
		_ = staged.Close()
		return err
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Rename(stagedName, target)
}

// currentExecutable is the running binary with symbolic links resolved.
func currentExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return executable, nil
}

// limitedOutput trims command output to its last few lines for an error.
func limitedOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	lines := strings.Split(text, "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	text = strings.Join(lines, " · ")
	if len(text) > 400 {
		text = text[len(text)-400:]
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
