package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestReplaceExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata")
	if err := os.WriteFile(path, []byte("before\nversion: old\nafter\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := replaceExactlyOnce(path, regexp.MustCompile(`(?m)^version: .+$`), []byte("version: 1.0.0")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "before\nversion: 1.0.0\nafter\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReplaceExactlyOnceRejectsAmbiguousInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata")
	if err := os.WriteFile(path, []byte("version: one\nversion: two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := replaceExactlyOnce(path, regexp.MustCompile(`(?m)^version: .+$`), []byte("version: 1.0.0"))
	if err == nil {
		t.Fatal("expected ambiguous input to fail")
	}
}

func TestConfigVersionReplacementPreservesWindowsLineEndings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	input := "version: \"3\"\r\n\r\ninfo:\r\n  productName: Faro\r\n  version: 0.9.0\r\n"
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := replaceExactlyOnce(path, configPattern, []byte("  version: 1.0.1")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "version: \"3\"\r\n\r\ninfo:\r\n  productName: Faro\r\n  version: 1.0.1\r\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
