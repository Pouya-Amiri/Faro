package mediaid

import (
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
