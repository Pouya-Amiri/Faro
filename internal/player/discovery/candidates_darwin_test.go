//go:build darwin

package discovery

import (
	"os"
	"path/filepath"
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

func TestExecutableAcceptsAppBundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "IINA.app")
	launcher := filepath.Join(bundle, "Contents", "MacOS", "iina-cli")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("test launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	if found, err := Executable("iina", bundle); err != nil || found != launcher {
		t.Fatalf("Executable(bundle) = %q, %v", found, err)
	}
}
