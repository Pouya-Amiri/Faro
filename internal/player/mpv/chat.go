package mpv

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/Pouya-Amiri/Faro/internal/player"
)

// chatOverlayID keeps Faro's overlay separate from any overlay a user script
// creates.
const chatOverlayID = 7201

// maxChatRunes keeps one message from covering the picture.
const maxChatRunes = 140

// ShowChat draws lines in the lower-left corner, above mpv's on-screen
// controller, with an osd-overlay so mpv's own OSD messages are unaffected.
func (m *MPV) ShowChat(ctx context.Context, lines []player.ChatLine) error {
	if len(lines) == 0 {
		return m.command(ctx, []any{"osd-overlay", chatOverlayID, "none", ""}, nil)
	}
	var events strings.Builder
	// The script space is 720 units high; \an1 anchors the block's bottom-left
	// corner so older lines stack upwards.
	events.WriteString(`{\an1\pos(28,610)\fs26\bord2\shad0\3c&H000000&\4a&HFF&}`)
	for index, line := range lines {
		if index > 0 {
			events.WriteString(`\N`)
		}
		events.WriteString(`{\b1\1c&HFFD37F&}`)
		events.WriteString(assEscape(line.Author))
		events.WriteString(`{\b0\1c&HFFFFFF&}  `)
		events.WriteString(assEscape(truncateRunes(line.Text, maxChatRunes)))
	}
	return m.command(ctx, []any{"osd-overlay", chatOverlayID, "ass-events", events.String(), 0, 720, 0}, nil)
}

// assEscape keeps chat text from being read as ASS override tags or line
// breaks.
func assEscape(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return strings.NewReplacer(`\`, "\\\u2060", "{", `\{`, "}", `\}`).Replace(text)
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}
