package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/player"
	"github.com/Pouya-Amiri/Faro/internal/protocol"
)

type overlayPlayer struct {
	*lifecyclePlayer
	mu    sync.Mutex
	shown [][]player.ChatLine
}

func (p *overlayPlayer) ShowChat(_ context.Context, lines []player.ChatLine) error {
	p.mu.Lock()
	p.shown = append(p.shown, append([]player.ChatLine(nil), lines...))
	p.mu.Unlock()
	return nil
}

func (p *overlayPlayer) last() ([]player.ChatLine, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.shown) == 0 {
		return nil, 0
	}
	return p.shown[len(p.shown)-1], len(p.shown)
}

func TestChatOverlayShowsRecentMessagesAndClearsThem(t *testing.T) {
	previous := chatOverlayDuration
	chatOverlayDuration = 300 * time.Millisecond
	t.Cleanup(func() { chatOverlayDuration = previous })

	service := New(context.Background(), nil)
	target := &overlayPlayer{lifecyclePlayer: newLifecyclePlayer()}
	service.player = target
	for index := range 6 {
		service.showChatInPlayer(&protocol.ChatMessage{ParticipantName: "Sam", Message: fmt.Sprintf("message %d", index)})
	}
	waitFor(t, "the overlay did not show the latest messages", func() bool {
		lines, _ := target.last()
		return len(lines) == chatOverlayLines && lines[chatOverlayLines-1].Text == "message 5" && lines[0].Text == "message 2"
	})
	waitFor(t, "the overlay was not cleared after the messages expired", func() bool {
		lines, count := target.last()
		return count > 0 && len(lines) == 0
	})

	service.SetChatOverlayEnabled(false)
	_, before := target.last()
	service.showChatInPlayer(&protocol.ChatMessage{ParticipantName: "Sam", Message: "hidden"})
	time.Sleep(50 * time.Millisecond)
	if _, after := target.last(); after != before {
		t.Fatal("a disabled overlay still drew a message")
	}
}

func waitFor(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
