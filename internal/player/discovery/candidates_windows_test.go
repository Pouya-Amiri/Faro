//go:build windows

package discovery

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestWindowsPlayerNamesAndChocolateyDefault(t *testing.T) {
	if !slices.Contains(platformExecutableNames("mpv.net"), "mpvnet.com") {
		t.Fatal("official mpv.net console launcher is missing")
	}
	if Supported("iina") {
		t.Fatal("IINA must not be offered on Windows")
	}
	t.Setenv("ChocolateyInstall", "")
	t.Setenv("ProgramData", `D:\ProgramData`)
	if got, want := chocolateyInstall(), filepath.Join(`D:\ProgramData`, "chocolatey"); got != want {
		t.Fatalf("chocolateyInstall() = %q, want %q", got, want)
	}
}
