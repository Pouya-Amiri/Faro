package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const packagedExecutable = "/usr/bin/faro"

func detectInstallation() installation {
	if os.Getenv("FLATPAK_ID") != "" || os.Getenv("SNAP") != "" || fileExists("/.flatpak-info") {
		return manualInstallation("Faro updates through your software center.")
	}
	if image := os.Getenv("APPIMAGE"); image != "" {
		// Replace the file itself, not a symbolic link pointing at it.
		if resolved, err := filepath.EvalSymlinks(image); err == nil {
			image = resolved
		}
		if info, err := os.Stat(image); err == nil && info.Mode().IsRegular() {
			if !writableDirectory(filepath.Dir(image)) {
				return manualInstallation("Faro can't replace its AppImage in " + filepath.Dir(image) + ". Download the new AppImage instead.")
			}
			return installation{method: MethodAppImage, target: image}
		}
	}
	executable, err := currentExecutable()
	if err != nil || executable != packagedExecutable {
		return manualInstallation("This copy of Faro wasn't installed from a Faro package, so it can't update itself.")
	}
	if fileExists("/run/ostree-booted") {
		return manualInstallation("Faro is layered into this system image. Update it with rpm-ostree.")
	}
	method := MethodManual
	switch {
	case ownedBy("faro:", "dpkg-query", "-S", executable):
		method = MethodDeb
	case ownedBy("faro-", "rpm", "-qf", executable):
		method = MethodRPM
	default:
		return manualInstallation("Faro's package isn't registered with the system package manager, so it can't update itself.")
	}
	if _, err := exec.LookPath("pkexec"); err != nil {
		return manualInstallation("Faro needs pkexec (polkit) to install updates. Install the new package with your package manager.")
	}
	if method == MethodDeb && firstCommand("apt-get", "dpkg") == "" || method == MethodRPM && firstCommand("dnf", "dnf5", "zypper", "rpm") == "" {
		return manualInstallation("No package manager was found to install the update with.")
	}
	return installation{method: method, target: executable}
}

func manualInstallation(reason string) installation {
	return installation{method: MethodManual, reason: reason}
}

func applyUpdate(ctx context.Context, current installation, file, _ string) (func() error, error) {
	switch current.method {
	case MethodAppImage:
		if err := replaceFile(file, current.target, 0o755); err != nil {
			return nil, fmt.Errorf("replace the AppImage: %w", err)
		}
		// The new AppImage's runtime describes its own mount and location.
		return relaunchExecutable(current.target, "APPIMAGE", "APPDIR", "ARGV0", "OWD"), nil
	case MethodDeb, MethodRPM:
		if err := installPackage(ctx, current.method, file); err != nil {
			return nil, err
		}
		return relaunchExecutable(current.target), nil
	}
	return nil, errors.New("this installation can't be updated automatically")
}

// installPackage installs a package file through polkit, which shows the
// desktop's own authentication dialog.
func installPackage(ctx context.Context, method Method, file string) error {
	var command []string
	switch method {
	case MethodDeb:
		if tool := firstCommand("apt-get"); tool != "" {
			// apt resolves any new dependency from the configured repositories.
			command = []string{tool, "install", "-y", "--no-install-recommends", file}
		} else {
			command = []string{firstCommand("dpkg"), "-i", file}
		}
	case MethodRPM:
		switch tool := firstCommand("dnf", "dnf5", "zypper", "rpm"); filepath.Base(tool) {
		case "dnf", "dnf5":
			command = []string{tool, "install", "-y", file}
		case "zypper":
			command = []string{tool, "--non-interactive", "install", "--allow-unsigned-rpm", file}
		default:
			command = []string{tool, "-U", "--replacepkgs", file}
		}
	}
	pkexec, err := exec.LookPath("pkexec")
	if err != nil || len(command) == 0 || command[0] == "" {
		return errors.New("no way to install the package was found")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, pkexec, command...).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 126:
		return errCancelled
	case errors.As(err, &exit) && exit.ExitCode() == 127:
		return errors.New("authentication failed, so the update was not installed")
	}
	if detail := limitedOutput(output); detail != "" {
		return fmt.Errorf("the package manager could not install the update: %s", detail)
	}
	return fmt.Errorf("the package manager could not install the update: %w", err)
}

func relaunchExecutable(path string, dropEnvironment ...string) func() error {
	return func() error {
		command := exec.Command(path)
		command.Env = relaunchEnvironment(dropEnvironment...)
		if home, err := os.UserHomeDir(); err == nil {
			command.Dir = home
		}
		return startDetached(command)
	}
}

// openDownload hands a package to the software center, or shows any other
// download in the file manager.
func openDownload(file string) error {
	target := file
	if ext := filepath.Ext(file); ext != ".deb" && ext != ".rpm" {
		target = filepath.Dir(file)
	}
	return startDetached(exec.Command("xdg-open", target))
}

func ownedBy(prefix, name string, args ...string) bool {
	path, err := exec.LookPath(name)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, args...).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(output)), prefix)
}

func firstCommand(names ...string) string {
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func cleanupInstallation() {}
