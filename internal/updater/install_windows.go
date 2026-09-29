package updater

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// innoUninstallKey is the uninstall entry Inno Setup writes for Faro's AppId.
const innoUninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\{E6F7A9B1-23C4-4B68-912E-73D41657DE02}_is1`

func detectInstallation() installation {
	executable, err := currentExecutable()
	if err != nil {
		return manualInstallation("Faro couldn't find where it is installed.")
	}
	directory := filepath.Dir(executable)
	if location, found := registeredInstall(registry.LOCAL_MACHINE); found && samePath(location, directory) {
		return installation{method: MethodWindowsInstaller, target: executable, allUsers: true}
	}
	if location, found := registeredInstall(registry.CURRENT_USER); found && samePath(location, directory) {
		return installation{method: MethodWindowsInstaller, target: executable}
	}
	if uninstallers, _ := filepath.Glob(filepath.Join(directory, "unins*.exe")); len(uninstallers) > 0 {
		return installation{method: MethodWindowsInstaller, target: executable, allUsers: underProgramFiles(directory)}
	}
	if !writableDirectory(directory) {
		return manualInstallation("Faro can't write to " + directory + ". Download the new version instead.")
	}
	return installation{method: MethodWindowsPortable, target: executable}
}

func manualInstallation(reason string) installation {
	return installation{method: MethodManual, reason: reason}
}

func registeredInstall(root registry.Key) (string, bool) {
	key, err := registry.OpenKey(root, innoUninstallKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", false
	}
	defer key.Close()
	location, _, err := key.GetStringValue("InstallLocation")
	return location, err == nil && location != ""
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func underProgramFiles(directory string) bool {
	for _, name := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if root := os.Getenv(name); root != "" {
			if relative, err := filepath.Rel(root, directory); err == nil && !strings.HasPrefix(relative, "..") {
				return true
			}
		}
	}
	return false
}

func applyUpdate(_ context.Context, current installation, file, _ string) (func() error, error) {
	switch current.method {
	case MethodWindowsInstaller:
		return runInstaller(current, file)
	case MethodWindowsPortable:
		return replacePortable(current, file)
	}
	return nil, errors.New("this installation can't be updated automatically")
}

// installerHelper runs after Faro has exited: it runs the installer, whose
// setup program asks for elevation itself when installing for all users, and
// then starts Faro again, updated or not. The installer is started through
// the shell so Windows can show its permission prompt, and it runs from
// Faro's own download, which has no mark of the web, so SmartScreen does not
// stop it.
const installerHelper = `$ErrorActionPreference = 'SilentlyContinue'
Wait-Process -Id ([int]$env:FARO_UPDATE_PID) -Timeout 60
try {
  Start-Process -FilePath $env:FARO_UPDATE_INSTALLER -ArgumentList $env:FARO_UPDATE_ARGUMENTS -Wait -ErrorAction Stop
} catch {}
Start-Process -FilePath $env:FARO_UPDATE_APP`

func runInstaller(current installation, installer string) (func() error, error) {
	directory := filepath.Dir(current.target)
	arguments := []string{"/SILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-", "/CLOSEAPPLICATIONS", `"/DIR=` + directory + `"`}
	if current.allUsers {
		arguments = append(arguments, "/ALLUSERS")
	} else {
		arguments = append(arguments, "/CURRENTUSER")
	}
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(powershell); err != nil {
		powershell = "powershell.exe"
	}
	return func() error {
		command := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-EncodedCommand", encodePowerShell(installerHelper))
		command.Env = append(os.Environ(),
			"FARO_UPDATE_PID="+strconv.Itoa(os.Getpid()),
			"FARO_UPDATE_INSTALLER="+installer,
			"FARO_UPDATE_ARGUMENTS="+strings.Join(arguments, " "),
			"FARO_UPDATE_APP="+current.target,
		)
		command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
		if err := command.Start(); err == nil {
			return command.Process.Release()
		}
		// Without PowerShell, start the installer directly; it closes Faro
		// through the Restart Manager if Faro has not exited yet.
		return shellExecute(installer, strings.Join(arguments, " "), directory)
	}, nil
}

func encodePowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, 0, len(encoded)*2)
	for _, unit := range encoded {
		bytes = append(bytes, byte(unit), byte(unit>>8))
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

func shellExecute(file, parameters, directory string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	target, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return err
	}
	params, err := windows.UTF16PtrFromString(parameters)
	if err != nil {
		return err
	}
	dir, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, target, params, dir, windows.SW_SHOWNORMAL)
}

// replacePortable swaps the new Faro.exe (and its notices) in beside the
// running one. Windows lets a running executable be renamed but not replaced,
// so the old one moves aside and is deleted on the next start.
func replacePortable(current installation, archive string) (func() error, error) {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return nil, fmt.Errorf("open the update: %w", err)
	}
	defer reader.Close()
	var executable *zip.File
	for _, entry := range reader.File {
		if strings.EqualFold(entry.Name, "Faro.exe") {
			executable = entry
		}
	}
	if executable == nil {
		return nil, errors.New("the update archive does not contain Faro.exe")
	}
	staged := current.target + ".new"
	if err := extractEntry(executable, staged); err != nil {
		return nil, fmt.Errorf("unpack the update: %w", err)
	}
	previous := current.target + ".old"
	_ = os.Remove(previous)
	if err := os.Rename(current.target, previous); err != nil {
		_ = os.Remove(staged)
		return nil, fmt.Errorf("replace Faro: %w", err)
	}
	if err := os.Rename(staged, current.target); err != nil {
		_ = os.Rename(previous, current.target)
		_ = os.Remove(staged)
		return nil, fmt.Errorf("replace Faro: %w", err)
	}
	directory := filepath.Dir(current.target)
	for _, entry := range reader.File {
		if name := entry.Name; strings.EqualFold(name, "LICENSE") || strings.EqualFold(name, "THIRD_PARTY_NOTICES.txt") {
			_ = extractEntry(entry, filepath.Join(directory, name))
		}
	}
	target := current.target
	return func() error {
		command := exec.Command(target)
		command.Dir = directory
		command.Env = relaunchEnvironment()
		return startDetached(command)
	}, nil
}

func extractEntry(entry *zip.File, target string) error {
	if entry.UncompressedSize64 > maximumDownload {
		return errors.New("archive entry is too large")
	}
	input, err := entry.Open()
	if err != nil {
		return err
	}
	defer input.Close()
	return writeFileAtomic(target, io.LimitReader(input, maximumDownload), 0o755)
}

// openDownload shows the download selected in Explorer.
func openDownload(file string) error {
	return startDetached(exec.Command("explorer.exe", "/select,", file))
}

// cleanupInstallation removes the executable a portable update moved aside.
func cleanupInstallation() {
	if executable, err := currentExecutable(); err == nil {
		_ = os.Remove(executable + ".old")
		_ = os.Remove(executable + ".new")
	}
}
