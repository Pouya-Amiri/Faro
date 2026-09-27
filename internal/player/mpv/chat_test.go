package mpv

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChatTextCannotInjectASS(t *testing.T) {
	got := assEscape("hi {\\b1}bold\\N\nnext")
	if want := "hi \\{\\\u2060b1\\}bold\\\u2060N next"; got != want {
		t.Fatalf("assEscape = %q, want %q", got, want)
	}
	long := truncateRunes(strings.Repeat("é", 200), maxChatRunes)
	if utf8.RuneCountInString(long) != maxChatRunes || !strings.HasSuffix(long, "…") {
		t.Fatalf("truncated message has %d runes: %q", utf8.RuneCountInString(long), long)
	}
}
