package mpv

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestWindowsExplicitConsoleLauncherPrefersSiblingExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows executable preference")
	}
	directory := t.TempDir()
	stub := filepath.Join(directory, "mpv.com")
	player := filepath.Join(directory, "mpv.exe")
	if err := os.WriteFile(stub, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(player, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := preferWindowsExecutable(ProfileMPV, stub); got != player {
		t.Fatalf("preferWindowsExecutable() = %q, want %q", got, player)
	}
}

func TestMPVNetUsesDedicatedProcessForFaroIPC(t *testing.T) {
	args := Config{Profile: ProfileMPVNet, ExtraArgs: []string{
		"--input-ipc-server=wrong", "--process-instance=single", "--idle=no", "--profile=fast",
	}}.arguments(`\\.\pipe\faro-test`, "")
	if !slices.Contains(args, "--process-instance=multi") {
		t.Fatalf("mpv.net arguments %v do not force a dedicated process", args)
	}
	for _, forbidden := range []string{"--input-ipc-server=wrong", "--process-instance=single", "--idle=no"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("mpv.net arguments retained protected override %q: %v", forbidden, args)
		}
	}
	if !slices.Contains(args, "--profile=fast") {
		t.Fatalf("mpv.net arguments dropped safe user option: %v", args)
	}
}
