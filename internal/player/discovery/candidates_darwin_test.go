//go:build darwin

package discovery

import (
	"slices"
	"testing"
)

func TestDarwinIINACandidatesUseDocumentedLauncherNames(t *testing.T) {
	names := platformExecutableNames("iina")
	if !slices.Contains(names, "iina") || !slices.Contains(names, "iina-cli") {
		t.Fatalf("IINA launcher names = %v", names)
	}
	if Supported("mpv.net") {
		t.Fatal("mpv.net must not be offered on macOS")
	}
}
