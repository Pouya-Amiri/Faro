//go:build !windows && !darwin

package discovery

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUnixCandidatesIncludeNixProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	want := filepath.Join(home, ".nix-profile", "bin", "mpv")
	if candidates := platformCandidates("mpv"); !slices.Contains(candidates, want) {
		t.Fatalf("platformCandidates(mpv) does not include %q: %v", want, candidates)
	}
}

func TestUnixCandidatesDoNotLaunchFlatpakInternals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, candidate := range platformCandidates("mpv") {
		if strings.Contains(candidate, string(filepath.Separator)+"flatpak"+string(filepath.Separator)+"app"+string(filepath.Separator)) {
			t.Fatalf("platformCandidates(mpv) included Flatpak-internal binary %q", candidate)
		}
	}
	if !Supported("mpv") || Supported("mpv.net") || Supported("iina") {
		t.Fatalf("unexpected Unix support matrix: %v", SupportedPlayers())
	}
}
