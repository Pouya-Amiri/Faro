package mediaid

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestFileFingerprintDoesNotDependOnPath(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first.mkv")
	second := filepath.Join(t.TempDir(), "renamed.mkv")
	content := []byte("same media bytes")
	if err := os.WriteFile(first, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, content, 0o600); err != nil {
		t.Fatal(err)
	}
	left, err := Inspect(first, "", 12)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Inspect(second, "", 12)
	if err != nil {
		t.Fatal(err)
	}
	if left.Media.Fingerprint != right.Media.Fingerprint {
		t.Fatalf("path changed fingerprint: %s != %s", left.Media.Fingerprint, right.Media.Fingerprint)
	}
	if left.Path == right.Path {
		t.Fatal("test paths unexpectedly match")
	}
}

func TestInspectAcceptsFileURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "My movie.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Inspect((&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != path {
		t.Fatalf("file URL resolved to %q, want %q", result.Path, path)
	}
}

func TestLocalPathFromWindowsFileURL(t *testing.T) {
	driveURL, err := url.Parse("file:///C:/Movies/My%20Film.mkv")
	if err != nil {
		t.Fatal(err)
	}
	path, err := localPathFromFileURL(driveURL, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if path != `C:\Movies\My Film.mkv` {
		t.Fatalf("drive file URL resolved to %q", path)
	}

	uncURL, err := url.Parse("file://server/share/My%20Film.mkv")
	if err != nil {
		t.Fatal(err)
	}
	path, err = localPathFromFileURL(uncURL, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if path != `\\server\share\My Film.mkv` {
		t.Fatalf("UNC file URL resolved to %q", path)
	}
}

func TestURLFingerprintIsCanonicalAndDropsFragment(t *testing.T) {
	left, err := Inspect("HTTPS://Example.COM/watch?v=1#chapter", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Inspect("https://example.com/watch?v=1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if left.Media.Fingerprint != right.Media.Fingerprint || left.URL != right.URL {
		t.Fatalf("URLs were not canonicalized: %#v %#v", left, right)
	}
}
