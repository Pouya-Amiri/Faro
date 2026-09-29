package updater

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func detectInstallation() installation {
	executable, err := currentExecutable()
	if err != nil {
		return manualInstallation("Faro couldn't find where it is installed.")
	}
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
	if filepath.Ext(bundle) != ".app" || filepath.Base(filepath.Dir(executable)) != "MacOS" {
		return manualInstallation("This copy of Faro isn't an app bundle, so it can't update itself.")
	}
	switch {
	case strings.Contains(bundle, "/AppTranslocation/"):
		// Gatekeeper runs a quarantined app that was never moved from a
		// read-only, randomised location.
		return manualInstallation("Move Faro into your Applications folder, then open it from there to enable automatic updates.")
	case strings.HasPrefix(bundle, "/Volumes/") && !writableDirectory(filepath.Dir(bundle)):
		return manualInstallation("Faro is running from its disk image. Drag it into Applications to enable automatic updates.")
	}
	return installation{method: MethodMacApp, target: bundle}
}

func manualInstallation(reason string) installation {
	return installation{method: MethodManual, reason: reason}
}

func applyUpdate(ctx context.Context, current installation, file, release string) (func() error, error) {
	if current.method != MethodMacApp {
		return nil, errors.New("this installation can't be updated automatically")
	}
	mount, err := os.MkdirTemp("", "faro-update-mount-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(mount)
	attach := exec.CommandContext(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-noautoopen", "-readonly", "-mountpoint", mount, file)
	if output, err := attach.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("open the update disk image: %s", firstNonEmpty(limitedOutput(output), err.Error()))
	}
	defer func() {
		detach := exec.Command("/usr/bin/hdiutil", "detach", mount)
		if detach.Run() != nil {
			_ = exec.Command("/usr/bin/hdiutil", "detach", "-force", mount).Run()
		}
	}()

	source := filepath.Join(mount, "Faro.app")
	if err := checkBundle(source, current.target, release); err != nil {
		return nil, err
	}
	// Stage and verify the new bundle beside the old one so the swap is two
	// renames on one volume. Without write access there, stage it privately
	// and swap with an administrator's permission.
	err = replaceBundle(ctx, source, current.target)
	if errors.Is(err, fs.ErrPermission) {
		err = replaceBundleAsAdministrator(ctx, source, current.target)
	}
	if err != nil {
		return nil, err
	}
	bundle := current.target
	return func() error {
		// open(1) goes through Launch Services like a normal launch; the shell
		// waits for this process first so the single-instance lock is free.
		script := `i=0; while /bin/kill -0 "$1" 2>/dev/null && [ "$i" -lt 300 ]; do /bin/sleep 0.2; i=$((i+1)); done; exec /usr/bin/open "$2"`
		return startDetached(exec.Command("/bin/sh", "-c", script, "sh", strconv.Itoa(os.Getpid()), bundle))
	}, nil
}

// checkBundle makes sure the disk image holds Faro, at the expected version.
func checkBundle(source, current, release string) error {
	if _, err := os.Stat(filepath.Join(source, "Contents", "MacOS")); err != nil {
		return errors.New("the update disk image does not contain Faro")
	}
	wantID := bundleValue(current, "CFBundleIdentifier")
	if id := bundleValue(source, "CFBundleIdentifier"); id == "" || wantID != "" && id != wantID {
		return fmt.Errorf("the update disk image contains an unexpected app (%s)", firstNonEmpty(id, "unknown"))
	}
	if got := bundleValue(source, "CFBundleShortVersionString"); got != "" {
		if parsed, err := parseVersion(got); err != nil || parsed.String() != release {
			return fmt.Errorf("the update disk image contains Faro %s instead of %s", got, release)
		}
	}
	return nil
}

func bundleValue(bundle, key string) string {
	output, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :"+key, filepath.Join(bundle, "Contents", "Info.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// stageBundle copies source to staged and checks its code signature. Files
// Faro downloads itself carry no quarantine attribute, so Gatekeeper does not
// stop the updated app; the attribute is cleared anyway in case the disk
// image was quarantined by some other route.
func stageBundle(ctx context.Context, source, staged string) error {
	if output, err := exec.CommandContext(ctx, "/usr/bin/ditto", source, staged).CombinedOutput(); err != nil {
		if strings.Contains(string(output), "Permission denied") {
			return fs.ErrPermission
		}
		return fmt.Errorf("copy the new version: %s", firstNonEmpty(limitedOutput(output), err.Error()))
	}
	if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", staged).CombinedOutput(); err != nil {
		return fmt.Errorf("the new version failed its signature check: %s", firstNonEmpty(limitedOutput(output), err.Error()))
	}
	_ = exec.Command("/usr/bin/xattr", "-dr", "com.apple.quarantine", staged).Run()
	return nil
}

func replaceBundle(ctx context.Context, source, target string) error {
	staging, err := os.MkdirTemp(filepath.Dir(target), ".faro-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	staged := filepath.Join(staging, "Faro.app")
	if err := stageBundle(ctx, source, staged); err != nil {
		return err
	}
	previous := filepath.Join(staging, "previous.app")
	if err := os.Rename(target, previous); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		if restoreErr := os.Rename(previous, target); restoreErr != nil {
			return fmt.Errorf("replace Faro: %v; restoring the previous version also failed: %v", err, restoreErr)
		}
		return err
	}
	// The running process keeps its own files open, so the previous bundle
	// can be removed (with the staging directory) straight away.
	return nil
}

// administratorSwap moves the old bundle aside, copies the new one into place
// and removes the old one, restoring it if the copy fails.
const administratorSwap = `on run argv
	set target to quoted form of item 1 of argv
	set staged to quoted form of item 2 of argv
	set previous to quoted form of ((item 1 of argv) & ".faro-previous")
	do shell script "/bin/rm -rf " & previous & " && /bin/mv " & target & " " & previous & " && if /usr/bin/ditto " & staged & " " & target & "; then /bin/rm -rf " & previous & "; else /bin/rm -rf " & target & "; /bin/mv " & previous & " " & target & "; exit 1; fi" with prompt "Faro wants to install an update." with administrator privileges
end run`

func replaceBundleAsAdministrator(ctx context.Context, source, target string) error {
	staging, err := os.MkdirTemp("", "faro-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	staged := filepath.Join(staging, "Faro.app")
	if err := stageBundle(ctx, source, staged); err != nil {
		return err
	}
	args := []string{}
	for _, line := range strings.Split(administratorSwap, "\n") {
		args = append(args, "-e", line)
	}
	args = append(args, target, staged)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/osascript", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if strings.Contains(string(output), "-128") {
		return errCancelled
	}
	return fmt.Errorf("replace Faro in %s: %s", filepath.Dir(target), firstNonEmpty(limitedOutput(output), err.Error()))
}

// openDownload opens the disk image, which mounts it in Finder.
func openDownload(file string) error {
	return startDetached(exec.Command("/usr/bin/open", file))
}

// cleanupInstallation removes staging directories left beside the bundle by
// an update that was interrupted.
func cleanupInstallation() {
	current := detectInstallation()
	if current.method != MethodMacApp {
		return
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(current.target), ".faro-update-*"))
	for _, leftover := range leftovers {
		_ = os.RemoveAll(leftover)
	}
}
