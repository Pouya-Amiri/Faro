package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogRotatesAndKeepsOnePreviousFile(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	line := strings.Repeat("x", 1000)
	for range 3000 {
		log.Printf("%s", line)
	}
	current, err := os.Stat(filepath.Join(dir, "faro.log"))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.Stat(filepath.Join(dir, "faro.log.1"))
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() > maxLogBytes || previous.Size() > maxLogBytes {
		t.Fatalf("log files grew past the limit: %d and %d bytes", current.Size(), previous.Size())
	}
}

func TestNilLogDiscards(t *testing.T) {
	var log *Log
	log.Printf("nothing %d", 1)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}
