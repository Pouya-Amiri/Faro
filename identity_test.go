package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityIsCreatedOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faro", "identity")
	first, err := loadIdentityFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !identityPattern.MatchString(first) {
		t.Fatalf("identity %q has the wrong form", first)
	}
	second, err := loadIdentityFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("the identity changed between launches")
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if replaced, err := loadIdentityFrom(path); err != nil || !identityPattern.MatchString(replaced) {
		t.Fatalf("a damaged identity was not replaced: %q, %v", replaced, err)
	}
}
