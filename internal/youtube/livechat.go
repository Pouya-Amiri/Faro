package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// chatPollInterval is how often the chat file yt-dlp writes is read.
var chatPollInterval = time.Second

// ChatMessage is one message of a YouTube live chat.
type ChatMessage struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Text   string `json:"text"`
	// Amount is a Super Chat's paid amount, as YouTube formats it.
	Amount       string `json:"amount,omitempty"`
	SentAtUnixMs int64  `json:"sentAtUnixMs"`
}

// ChatUpdate is what arrived from a live chat since the previous update.
type ChatUpdate struct {
	Messages []ChatMessage `json:"messages,omitempty"`
	// Removed lists messages moderators deleted.
	Removed []string `json:"removed,omitempty"`
}

var errNoLiveChat = errors.New("this broadcast has no live chat")

// LiveChat follows the live chat of the broadcast at source, read only,
// until ctx ends or the chat does, passing what arrives to deliver. yt-dlp
// writes the chat as JSON lines to a file for as long as the broadcast
// lasts; LiveChat reads the lines as they appear.
func (r *Resolver) LiveChat(ctx context.Context, source string, deliver func(ChatUpdate)) error {
	executable, runtimeArg, err := r.tools(ctx)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "faro-chat-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	args := []string{
		"--ignore-config", "--no-warnings", "--no-progress", "--no-colors",
		"--skip-download", "--write-subs", "--sub-langs", "live_chat",
		"--output", filepath.Join(dir, "chat.%(ext)s"),
	}
	args = append(args, runtimeArg...)
	args = append(args, "--remote-components", "ejs:github", "--", source)
	command := newCommandContext(ctx, executable, args...)
	var stderr boundedBuffer
	command.Stderr = &stderr
	// A helper yt-dlp started (its JavaScript runtime) may outlive it and
	// hold stderr open; stopping must not wait for that.
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	tail := chatTail{dir: dir}
	ticker := time.NewTicker(chatPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			<-exited
			return ctx.Err()
		case <-ticker.C:
			tail.read(deliver)
		case exitErr := <-exited:
			tail.read(deliver)
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case exitErr != nil:
				return errors.New(firstNonEmpty(conciseError(errors.New(stderr.String())), exitErr.Error()))
			case !tail.found:
				return errNoLiveChat
			}
			return nil
		}
	}
}

// chatTail reads the lines appended to yt-dlp's chat file. While the
// broadcast lasts the file has a .part suffix; yt-dlp renames it when the
// chat ends, so reading continues at the same offset in the final name.
type chatTail struct {
	dir    string
	offset int64
	found  bool
}

func (t *chatTail) read(deliver func(ChatUpdate)) {
	for _, name := range []string{"chat.live_chat.json.part", "chat.live_chat.json"} {
		file, err := os.Open(filepath.Join(t.dir, name))
		if err != nil {
			continue
		}
		t.found = true
		data, _ := io.ReadAll(io.NewSectionReader(file, t.offset, 1<<62))
		file.Close()
		// Only complete lines; the last one may still be being written.
		end := bytes.LastIndexByte(data, '\n')
		if end < 0 {
			return
		}
		t.offset += int64(end + 1)
		var update ChatUpdate
		for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
			parseChatLine(line, &update)
		}
		if len(update.Messages) != 0 || len(update.Removed) != 0 {
			deliver(update)
		}
		return
	}
}

// chatLine is the part of a yt-dlp live chat line Faro reads.
type chatLine struct {
	Replay struct {
		Actions []struct {
			Add *struct {
				Item map[string]chatRenderer `json:"item"`
			} `json:"addChatItemAction"`
			Remove *struct {
				TargetItemID string `json:"targetItemId"`
			} `json:"removeChatItemAction"`
		} `json:"actions"`
	} `json:"replayChatItemAction"`
}

type chatRenderer struct {
	ID         string `json:"id"`
	AuthorName struct {
		SimpleText string `json:"simpleText"`
	} `json:"authorName"`
	Message struct {
		Runs []struct {
			Text  string `json:"text"`
			Emoji *struct {
				EmojiID   string   `json:"emojiId"`
				Shortcuts []string `json:"shortcuts"`
				IsCustom  bool     `json:"isCustomEmoji"`
			} `json:"emoji"`
		} `json:"runs"`
	} `json:"message"`
	PurchaseAmountText struct {
		SimpleText string `json:"simpleText"`
	} `json:"purchaseAmountText"`
	TimestampUsec string `json:"timestampUsec"`
}

// parseChatLine adds the text messages and deletions in line to update.
// Other chat events (memberships, polls, banners) are skipped.
func parseChatLine(line []byte, update *ChatUpdate) {
	var parsed chatLine
	if json.Unmarshal(line, &parsed) != nil {
		return
	}
	for _, action := range parsed.Replay.Actions {
		if action.Remove != nil && action.Remove.TargetItemID != "" {
			update.Removed = append(update.Removed, action.Remove.TargetItemID)
		}
		if action.Add == nil {
			continue
		}
		for kind, renderer := range action.Add.Item {
			if kind != "liveChatTextMessageRenderer" && kind != "liveChatPaidMessageRenderer" {
				continue
			}
			if message, ok := renderer.message(); ok {
				update.Messages = append(update.Messages, message)
			}
		}
	}
}

func (r chatRenderer) message() (ChatMessage, bool) {
	var text strings.Builder
	for _, run := range r.Message.Runs {
		switch {
		case run.Emoji == nil:
			text.WriteString(run.Text)
		case run.Emoji.IsCustom && len(run.Emoji.Shortcuts) > 0:
			// A channel's own emoji has no character; show its shortcut.
			text.WriteString(run.Emoji.Shortcuts[0])
		default:
			text.WriteString(run.Emoji.EmojiID)
		}
	}
	message := ChatMessage{
		ID: r.ID, Author: strings.TrimSpace(r.AuthorName.SimpleText),
		Text: strings.TrimSpace(text.String()), Amount: r.PurchaseAmountText.SimpleText,
	}
	if micros, err := strconv.ParseInt(r.TimestampUsec, 10, 64); err == nil {
		message.SentAtUnixMs = micros / 1000
	}
	return message, message.ID != "" && (message.Text != "" || message.Amount != "")
}

// boundedBuffer keeps the last few kilobytes written to it.
type boundedBuffer struct{ data []byte }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	const limit = 4 * 1024
	b.data = append(b.data, data...)
	if len(b.data) > limit {
		b.data = b.data[len(b.data)-limit:]
	}
	return len(data), nil
}

func (b *boundedBuffer) String() string { return strings.TrimSpace(string(b.data)) }
