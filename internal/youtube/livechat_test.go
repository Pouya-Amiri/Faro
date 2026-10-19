package youtube

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const chatTextLine = `{"replayChatItemAction": {"actions": [{"clickTrackingParams": "x", "addChatItemAction": {"item": {"liveChatTextMessageRenderer": {"message": {"runs": [{"text": "hello "}, {"emoji": {"emojiId": "🎃", "shortcuts": [":jack_o_lantern:"]}}, {"text": " "}, {"emoji": {"emojiId": "UCabc/xyz", "shortcuts": [":channel-wave:"], "isCustomEmoji": true}}]}, "authorName": {"simpleText": "@viewer"}, "id": "message-1", "timestampUsec": "1792392001904219"}}, "clientId": "c"}}]}, "videoOffsetTimeMsec": "0", "isLive": true}`

const chatPaidLine = `{"replayChatItemAction": {"actions": [{"addChatItemAction": {"item": {"liveChatPaidMessageRenderer": {"id": "paid-1", "authorName": {"simpleText": "@fan"}, "purchaseAmountText": {"simpleText": "€5.00"}, "timestampUsec": "1792392002000000"}}}}]}, "isLive": true}`

const chatOtherLines = `{"replayChatItemAction": {"actions": [{"removeChatItemAction": {"targetItemId": "message-0"}}, {"addChatItemAction": {"item": {"liveChatViewerEngagementMessageRenderer": {"id": "notice"}}}}, {"addBannerToLiveChatCommand": {}}]}, "isLive": true}`

func TestParseChatLineReadsMessagesAndDeletions(t *testing.T) {
	var update ChatUpdate
	for _, line := range []string{chatTextLine, chatPaidLine, chatOtherLines, "not json"} {
		parseChatLine([]byte(line), &update)
	}
	if len(update.Messages) != 2 {
		t.Fatalf("parsed %d messages, want 2: %#v", len(update.Messages), update.Messages)
	}
	text := update.Messages[0]
	if text.ID != "message-1" || text.Author != "@viewer" || text.Text != "hello 🎃 :channel-wave:" || text.SentAtUnixMs != 1792392001904 {
		t.Fatalf("unexpected text message: %#v", text)
	}
	if paid := update.Messages[1]; paid.ID != "paid-1" || paid.Amount != "€5.00" || paid.Text != "" {
		t.Fatalf("unexpected Super Chat: %#v", paid)
	}
	if len(update.Removed) != 1 || update.Removed[0] != "message-0" {
		t.Fatalf("unexpected deletions: %#v", update.Removed)
	}
}

// fakeChatTool writes a script that behaves like yt-dlp following a live
// chat: it appends lines to the .part file, then renames it and exits.
func fakeChatTool(t *testing.T, body string) *Resolver {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as yt-dlp")
	}
	script := filepath.Join(t.TempDir(), "yt-dlp")
	content := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output ] && out=\"$2\"; shift; done\n" +
		"file=$(printf %s \"$out\" | sed 's/%(ext)s/live_chat.json/')\n" + body
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Resolver{executable: script, runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"}}
}

func TestLiveChatFollowsTheGrowingChatFile(t *testing.T) {
	previous := chatPollInterval
	chatPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { chatPollInterval = previous })
	resolver := fakeChatTool(t, "cat >> \"$file.part\" <<'EOF'\n"+chatTextLine+"\nEOF\nsleep 0.2\n"+
		"cat >> \"$file.part\" <<'EOF'\n"+chatPaidLine+"\n"+chatOtherLines+"\nEOF\nsleep 0.2\nmv \"$file.part\" \"$file\"\n")
	var updates []ChatUpdate
	err := resolver.LiveChat(context.Background(), "https://www.youtube.com/watch?v=live", func(update ChatUpdate) {
		updates = append(updates, update)
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	removed := 0
	for _, update := range updates {
		for _, message := range update.Messages {
			ids = append(ids, message.ID)
		}
		removed += len(update.Removed)
	}
	if strings.Join(ids, ",") != "message-1,paid-1" || removed != 1 || len(updates) < 2 {
		t.Fatalf("unexpected updates %v (%d updates, %d removed)", ids, len(updates), removed)
	}
}

func TestLiveChatReportsABroadcastWithoutChat(t *testing.T) {
	resolver := fakeChatTool(t, "echo 'There are no subtitles for the requested languages'\n")
	if err := resolver.LiveChat(context.Background(), "https://www.youtube.com/watch?v=quiet", func(ChatUpdate) {}); err != errNoLiveChat {
		t.Fatalf("unexpected result %v", err)
	}
}

func TestLiveChatStopsWithItsContext(t *testing.T) {
	resolver := fakeChatTool(t, "touch \"$file.part\"\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := resolver.LiveChat(ctx, "https://www.youtube.com/watch?v=live", func(ChatUpdate) {}); err == nil {
		t.Fatal("a cancelled chat reported success")
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("the chat kept running %s after its context ended", elapsed)
	}
}

func TestRateLimitIsNotRetriedWithAnotherClient(t *testing.T) {
	calls := 0
	resolver := &Resolver{
		executable: "yt-dlp", runtimeArg: []string{"--js-runtimes", "node:/usr/bin/node"},
		run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, errorString("ERROR: [youtube] abc: Unable to download webpage: HTTP Error 429: Too Many Requests")
		},
	}
	if _, err := resolver.Resolve(context.Background(), "https://youtu.be/abc", 0); err != errRateLimited || calls != 1 {
		t.Fatalf("a rate limit made %d requests and returned %v", calls, err)
	}
}

func TestInfoReportsLiveBroadcasts(t *testing.T) {
	if !infoFrom(extractOutput{Title: "Live", LiveStatus: "is_live"}, "").Live || !infoFrom(extractOutput{Title: "Live", IsLive: true}, "").Live {
		t.Fatal("a live broadcast was not reported as live")
	}
	if infoFrom(extractOutput{Title: "Replay", IsLive: true, LiveStatus: "was_live"}, "").Live {
		t.Fatal("a finished broadcast was reported as live")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
